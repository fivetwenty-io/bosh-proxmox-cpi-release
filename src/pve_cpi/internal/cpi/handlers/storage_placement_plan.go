package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	rank "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
)

// StoragePlanErrorKind classifies failures before a storage mutation.
type StoragePlanErrorKind string

// Storage plan error kinds distinguish invalid policy, infeasibility, and observation failures.
const (
	StoragePlanConfiguration  StoragePlanErrorKind = "configuration"
	StoragePlanObservation    StoragePlanErrorKind = "observation"
	StoragePlanCapacity       StoragePlanErrorKind = "capacity"
	StoragePlanBudget         StoragePlanErrorKind = "search_budget"
	StoragePlanReconciliation StoragePlanErrorKind = "reconciliation"
)

// StoragePlanError carries a safe failure category and operator-facing reason.
type StoragePlanError struct {
	Kind   StoragePlanErrorKind
	Detail string
}

func (e *StoragePlanError) Error() string {
	return "storage placement " + string(e.Kind) + ": " + e.Detail
}

// CPI never advertises replay of an uncertain create_disk as safe.
func (e *StoragePlanError) CPI() *cpierrors.Error {
	if e.Kind == StoragePlanObservation || e.Kind == StoragePlanCapacity || e.Kind == StoragePlanBudget {
		return cpierrors.Retriable("%s", e)
	}
	return cpierrors.Cloud("%s", e)
}
func planError(k StoragePlanErrorKind, f string, a ...any) error {
	return &StoragePlanError{k, fmt.Sprintf(f, a...)}
}

// StoragePlanNodeGroup preserves compute-score order within an AZ priority group.
type StoragePlanNodeGroup struct {
	AZ      string
	Nodes   []string
	HANodes []string
}

// StorageRootSource records source facts; a zero TemplateVMID identifies a direct import.
// ScratchBytes is additional temporary allocation on the selected target.
type StorageRootSource struct {
	Node, StorageID, VolumeID  string
	TemplateVMID               int
	VirtualBytes, ScratchBytes uint64
	AuxiliaryBytes             uint64
	AuxiliaryVolumes           []StorageExistingVolume
}

// StorageExistingVolume constrains placement to nodes that can reach an existing disk.
type StorageExistingVolume struct {
	StorageID, Node, VolumeID, Device string
	VirtualBytes                      uint64
}

// StoragePlanRequest supplies allocation identity, costs, and compute constraints.
type StoragePlanRequest struct {
	// OnCandidateRejected receives the fixed role of a feasibility exclusion.
	OnCandidateRejected                                  func(string) `json:"-"`
	Selection                                            *StoragePlacementSelection
	Inventory                                            *inv.Snapshot
	Groups                                               []StoragePlanNodeGroup
	Namespace, AllocationKey                             string
	Seed                                                 [32]byte
	SeedSet                                              bool
	RootBytes, EphemeralBytes, PersistentBytes, ISOBytes uint64
	Sources                                              []StorageRootSource
	CloneMode                                            string
	OriginalISOStorage                                   string
	ISOFollowRoot, RequireSharedISO                      bool
	Existing                                             []StorageExistingVolume
	HANodes                                              []string
	ExtraLimits                                          inv.Limits
	RoleLimits                                           map[string]inv.Limits
	SearchBudget                                         int
	Clock                                                func() time.Time
}

// StoragePlanTarget freezes one role's physical destination and virtual size.
type StoragePlanTarget struct {
	Role, Node, StorageID, BackingKey, CapacityKey, DomainKey string
	Mechanism                                                 string
	VirtualBytes, ChargeBytes                                 uint64
	Source                                                    *StorageRootSource `json:",omitempty"`
	Observation                                               inv.ObservationStamp
}

// StorageAllocationPlan retains the complete storage and compute decision for replay.
type StorageAllocationPlan struct {
	VMExecution                                           *StorageVMExecution `json:",omitempty"`
	Version                                               int
	Namespace, AllocationKey, PolicyFingerprint, AZ, Node string
	Seed                                                  [32]byte
	HANodes                                               []string
	Targets                                               []StoragePlanTarget
	Charges                                               []inv.ChargeRecord
	Rankings                                              []rank.RankedCandidate
	Strategies                                            map[string]rank.Policy
	Definitions                                           map[string]pve.StorageInfo
	CapacityDomains                                       map[string][]string
	Rejections                                            []string
}
type storagePlanOption struct {
	target    StoragePlanTarget
	ledger    inv.Ledger
	candidate rank.EligibleCandidate
	ranking   rank.RankedCandidate
}

// StoragePlanIterator enumerates feasible plans until a remote mutation is submitted.
type StoragePlanIterator struct {
	req               StoragePlanRequest
	group             int
	roots             []storagePlanOption
	rootIndex         int
	continuations     []storagePlanOption
	continuationIndex int
	activeRoot        *storagePlanOption
	rejections        []string
	examined          int
	submitted         bool
	fingerprint       string
}

// NewStoragePlanIterator validates frozen inputs before constructing feasible choices.
func NewStoragePlanIterator(r StoragePlanRequest) (*StoragePlanIterator, error) {
	if r.Selection == nil || r.Selection.Policy == nil || r.Inventory == nil {
		return nil, planError(StoragePlanConfiguration, "selection and inventory are required")
	}
	if r.AllocationKey == "" || r.Namespace == "" {
		return nil, planError(StoragePlanConfiguration, "allocation identity is required")
	}
	if r.SearchBudget <= 0 {
		return nil, planError(StoragePlanConfiguration, "positive search budget is required")
	}
	if r.Clock == nil {
		r.Clock = time.Now
	}
	if r.CloneMode == "" {
		r.CloneMode = "auto"
	}
	if r.CloneMode != "auto" && r.CloneMode != "full" && r.CloneMode != "linked" {
		return nil, planError(StoragePlanConfiguration, "unknown clone mode")
	}
	for _, size := range []*uint64{&r.RootBytes, &r.EphemeralBytes, &r.PersistentBytes} {
		rounded, err := inv.RoundBytes(*size, 1<<30)
		if err != nil {
			return nil, planError(StoragePlanConfiguration, "disk rounding: %v", err)
		}
		*size = rounded
	}
	if !r.SeedSet {
		seed, err := rank.NewSeed()
		if err != nil {
			return nil, err
		}
		r.Seed = seed
		r.SeedSet = true
	}
	// Round-trip only the in-memory selector snapshot, never its credentials to disk.
	raw, err := json.Marshal(r.Selection)
	if err != nil {
		return nil, err
	}
	var selected StoragePlacementSelection
	if err := json.Unmarshal(raw, &selected); err != nil {
		return nil, err
	}
	r.Selection = &selected
	r.Groups = slices.Clone(r.Groups)
	for n := range r.Groups {
		r.Groups[n].Nodes = slices.Clone(r.Groups[n].Nodes)
		r.Groups[n].HANodes = slices.Clone(r.Groups[n].HANodes)
	}
	r.Sources = slices.Clone(r.Sources)
	for n := range r.Sources {
		r.Sources[n].AuxiliaryVolumes = slices.Clone(r.Sources[n].AuxiliaryVolumes)
	}
	r.Existing = slices.Clone(r.Existing)
	r.HANodes = slices.Clone(r.HANodes)
	roleLimits := make(map[string]inv.Limits, len(r.RoleLimits))
	for k, v := range r.RoleLimits {
		roleLimits[k] = v
	}
	r.RoleLimits = roleLimits
	if len(r.Groups) == 0 {
		return nil, planError(StoragePlanCapacity, "no compute-eligible nodes")
	}
	roles := []*StorageRoleSelection{selected.Root, selected.Ephemeral, selected.Persistent}
	for _, role := range roles {
		if role != nil {
			if _, err = planRoleMembers(r.Inventory, *role); err != nil {
				return nil, err
			}
		}
	}
	if selected.Root != nil && len(r.Sources) == 0 {
		return nil, planError(StoragePlanObservation, "no observed root source")
	}
	for _, source := range r.Sources {
		if source.VirtualBytes == 0 || source.StorageID == "" || source.VolumeID == "" || source.Node == "" {
			return nil, planError(StoragePlanObservation, "incomplete root source facts")
		}
	}
	if selected.Ephemeral != nil && r.EphemeralBytes == 0 {
		return nil, planError(StoragePlanConfiguration, "dedicated ephemeral disk must have positive size")
	}
	if selected.Persistent != nil && r.PersistentBytes == 0 {
		return nil, planError(StoragePlanConfiguration, "persistent disk must have positive size")
	}
	// Only explicitly allowlisted placement policy enters the durable fingerprint.
	policy := struct {
		Roles          []*StorageRoleSelection
		Future         string
		Domains        any
		ISO            string
		Follow, Shared bool
		Limits         inv.Limits
		RoleLimits     map[string]inv.Limits
	}{roles, selected.FuturePersistentSet, selected.Policy.StorageCapacityDomains, r.OriginalISOStorage, r.ISOFollowRoot, r.RequireSharedISO, r.ExtraLimits, r.RoleLimits}
	raw, err = json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	return &StoragePlanIterator{req: r, fingerprint: hex.EncodeToString(digest[:])}, nil
}
func planRoleMembers(s *inv.Snapshot, r StorageRoleSelection) ([]string, error) {
	var ids []string
	switch r.Kind {
	case storageSelectorSet:
		var ok bool
		ids, ok = s.Members(r.Value)
		if !ok {
			return nil, planError(StoragePlanConfiguration, "set %q was not discovered", r.Value)
		}
	case storageSelectorPool:
		ids = []string{r.Value}
	case storageSelectorTier:
		id, ok := s.TierStorage(r.Value)
		if !ok {
			return nil, planError(StoragePlanConfiguration, "tier %q was not discovered", r.Value)
		}
		ids = []string{id}
	default:
		return nil, planError(StoragePlanConfiguration, "unknown role selector kind %q", r.Kind)
	}
	if len(ids) == 0 {
		return nil, planError(StoragePlanConfiguration, "empty %s membership", r.Role)
	}
	for _, id := range ids {
		if _, ok := s.Definition(id); !ok {
			return nil, planError(StoragePlanConfiguration, "storage %q absent from frozen discovery", id)
		}
	}
	if r.BoundaryName != "" {
		boundary, ok := s.Members(r.BoundaryName)
		if !ok {
			return nil, planError(StoragePlanConfiguration, "boundary not discovered")
		}
		if err := ValidateStoragePlacementBoundary(r, ids, boundary); err != nil {
			return nil, planError(StoragePlanConfiguration, "%v", err)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// MarkSubmitted prevents selecting an alternate plan after a remote submission.
func (i *StoragePlanIterator) MarkSubmitted() { i.submitted = true }
func (i *StoragePlanIterator) guard(ctx context.Context) error {
	if i.submitted {
		return planError(StoragePlanReconciliation, "submitted plan cannot be replaced")
	}
	if err := ctx.Err(); err != nil {
		return planError(StoragePlanObservation, "%v", err)
	}
	if !i.req.Inventory.Fresh(i.req.Clock()) {
		return planError(StoragePlanObservation, "frozen observations expired; refresh before a new planning attempt")
	}
	return nil
}
func (i *StoragePlanIterator) consume(ctx context.Context) error {
	if err := i.guard(ctx); err != nil {
		return err
	}
	if i.examined >= i.req.SearchBudget {
		return planError(StoragePlanBudget, "examined %d candidates without proving exhaustion", i.examined)
	}
	i.examined++
	return nil
}

// Next returns the next feasible plan without submitting remote work.
func (i *StoragePlanIterator) Next(ctx context.Context) (*StorageAllocationPlan, error) {
	if err := i.guard(ctx); err != nil {
		return nil, err
	}
	for i.group < len(i.req.Groups) {
		if i.activeRoot != nil && i.continuationIndex < len(i.continuations) {
			e := i.continuations[i.continuationIndex]
			i.continuationIndex++
			return i.makePlan(*i.activeRoot, &e), nil
		}
		i.activeRoot = nil
		i.continuations = nil
		i.continuationIndex = 0
		if i.roots == nil {
			role := i.req.Selection.Root
			bytes := i.req.RootBytes
			if role == nil {
				role = i.req.Selection.Persistent
				bytes = i.req.PersistentBytes
			}
			if role == nil {
				return nil, planError(StoragePlanConfiguration, "operation creates no storage roles")
			}
			options, err := i.options(ctx, *role, bytes, inv.NewLedger(), i.req.Groups[i.group].Nodes, true)
			if err != nil {
				return nil, err
			}
			i.roots = options
		}
		for i.rootIndex < len(i.roots) {
			root := i.roots[i.rootIndex]
			i.rootIndex++
			if i.req.Selection.Ephemeral == nil {
				return i.makePlan(root, nil), nil
			}
			if i.req.Selection.BundleVMDisks {
				e, err := i.addTarget(root.ledger, storageRoleEphemeral, root.target.Node, root.target.StorageID, i.req.EphemeralBytes, i.req.EphemeralBytes, "allocate", nil, *i.req.Selection.Ephemeral)
				if err != nil {
					i.rejectCandidate(storageRoleEphemeral, err)
					continue
				}
				return i.makePlan(root, &e), nil
			}
			ep, err := i.options(ctx, *i.req.Selection.Ephemeral, i.req.EphemeralBytes, root.ledger, []string{root.target.Node}, false)
			if err != nil {
				return nil, err
			}
			if len(ep) == 0 {
				continue
			}
			i.activeRoot = &root
			i.continuations = ep
			i.continuationIndex = 1
			return i.makePlan(root, &ep[0]), nil
		}
		i.group++
		i.roots = nil
		i.rootIndex = 0
	}
	return nil, planError(StoragePlanCapacity, "no complete feasible allocation plan; %s", strings.Join(i.rejections, "; "))
}
func (i *StoragePlanIterator) limits(r StorageRoleSelection) (inv.Limits, error) {
	if r.ReserveMB < 0 || uint64(r.ReserveMB) > math.MaxUint64/(1<<20) {
		return inv.Limits{}, planError(StoragePlanConfiguration, "reserve overflow")
	}
	limits := inv.Limits{ReserveBytes: uint64(r.ReserveMB) * (1 << 20)}
	if r.CeilingPct != nil {
		limits.MaxUtilizationPct = *r.CeilingPct
	}
	return inv.CombineLimits(limits, i.req.ExtraLimits, i.req.RoleLimits[r.Role])
}
func (i *StoragePlanIterator) accessible(node, id string) error {
	p, ok := i.req.Inventory.Pair(node, id)
	if !ok || p.Reason != "" {
		return fmt.Errorf("storage %s is not eligible on %s", id, node)
	}
	return nil
}
func planContent(d pve.StorageInfo, content string) bool {
	if d.Disabled {
		return false
	}
	switch strings.ToLower(d.Type) {
	case "dir", "nfs", "cifs", "cephfs", "glusterfs", "btrfs":
	case "lvm", "lvmthin", "zfspool", "rbd", "iscsi", "iscsidirect", "zfs":
		if content != "images" {
			return false
		}
	default:
		return false
	}
	for _, c := range strings.Split(d.Content, ",") {
		if strings.TrimSpace(c) == content {
			return true
		}
	}
	return false
}
func (i *StoragePlanIterator) accessConstraints(node, id string) error {
	d, ok := i.req.Inventory.Definition(id)
	if !ok || !planContent(d, "images") {
		return fmt.Errorf("disk target %s lacks supported images backend/content", id)
	}
	if err := i.accessible(node, id); err != nil {
		return err
	}
	for _, ha := range i.haNodes() {
		if err := i.accessible(ha, id); err != nil {
			return err
		}
	}
	for _, existing := range i.req.Existing {
		d, ok := i.req.Inventory.Definition(existing.StorageID)
		if !ok {
			return fmt.Errorf("existing disk definition absent")
		}
		if !d.IsShared() && existing.Node != node {
			return fmt.Errorf("existing disk pins node %s", existing.Node)
		}
		if err := i.accessible(node, existing.StorageID); err != nil {
			return err
		}
	}
	if set := i.req.Selection.FuturePersistentSet; set != "" {
		members, ok := i.req.Inventory.Members(set)
		if !ok {
			return fmt.Errorf("future P membership absent")
		}
		found := false
		for _, member := range members {
			if i.accessible(node, member) == nil {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no future P member accessible on %s", node)
		}
	}
	return nil
}
func (i *StoragePlanIterator) sourceFor(node, id string) (*StorageRootSource, string, error) {
	target, _ := i.req.Inventory.Definition(id)
	var candidates []StorageRootSource
	for _, s := range i.req.Sources {
		def, ok := i.req.Inventory.Definition(s.StorageID)
		content := "images"
		if s.TemplateVMID == 0 {
			content = "import"
		}
		if !ok || !planContent(def, content) || !def.IsShared() && s.Node != node {
			continue
		}
		if i.accessible(node, s.StorageID) != nil {
			continue
		}
		auxOK := i.sourceAuxiliaryAccessible(node, s.AuxiliaryVolumes)
		if auxOK {
			candidates = append(candidates, s)
		}
	}
	sameTarget := func(s StorageRootSource) bool {
		if s.TemplateVMID == 0 {
			return false
		}
		if s.StorageID == id {
			return true
		}
		def, ok := i.req.Inventory.Definition(s.StorageID)
		return ok && pve.SameBacking(def, target)
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		x, y := candidates[a], candidates[b]
		// Every template outranks every import candidate, so a target that
		// is also the stemcell pool still clones from a template elsewhere
		// instead of importing the qcow2 in full.
		if xt, yt := x.TemplateVMID > 0, y.TemplateVMID > 0; xt != yt {
			return xt
		}
		// A template already on the target storage clones linked; rank it
		// ahead of every other template regardless of VMID.
		if sx, sy := sameTarget(x), sameTarget(y); sx != sy {
			return sx
		}
		if x.TemplateVMID != y.TemplateVMID {
			return x.TemplateVMID > y.TemplateVMID
		}
		if x.StorageID != y.StorageID {
			return x.StorageID < y.StorageID
		}
		return x.Node < y.Node
	})
	for _, s := range candidates {
		if s.TemplateVMID == 0 {
			if i.req.CloneMode != "linked" {
				cloned := s
				return &cloned, "import", nil
			}
			continue
		}
		source, _ := i.req.Inventory.Definition(s.StorageID)
		linked := pve.SameBacking(source, target) && pve.IsLinkedCloneSupported(source.Type)
		// A linked clone cannot redirect a disk's storage ID. Every cloned overlay
		// must land on the recorded target; choose a full clone before ranking when
		// source aliases or auxiliary disks would place it elsewhere.
		linked = linked && s.StorageID == id
		for _, aux := range s.AuxiliaryVolumes {
			if aux.StorageID != id {
				linked = false
			}
		}

		if i.req.CloneMode == "linked" && !linked {
			continue
		}
		mode := storageMechanismFullClone
		if linked && i.req.CloneMode != "full" {
			mode = "linked_clone"
		}
		cloned := s
		return &cloned, mode, nil
	}
	return nil, "", fmt.Errorf("no compatible observed root source on %s for %s", node, id)
}
func (i *StoragePlanIterator) addTarget(ledger inv.Ledger, role, node, id string, virtual, bytes uint64, mechanism string, source *StorageRootSource, r StorageRoleSelection) (storagePlanOption, error) {
	limits, err := i.limits(r)
	if err != nil {
		return storagePlanOption{}, err
	}
	candidate, err := ledger.Candidate(i.req.Inventory, node, id, bytes, limits)
	if err != nil {
		return storagePlanOption{}, err
	}
	next, err := ledger.WithPlanned(i.req.Inventory, inv.Charge{ID: role, Role: role, Node: node, StorageID: id, Bytes: bytes, Limits: limits})
	if err != nil {
		return storagePlanOption{}, err
	}
	p, _ := i.req.Inventory.Pair(node, id)
	return storagePlanOption{target: StoragePlanTarget{Role: role, Node: node, StorageID: id, BackingKey: p.BackingKey, CapacityKey: p.CapacityKey, DomainKey: i.req.Inventory.DomainForStorage(id), Mechanism: mechanism, VirtualBytes: virtual, ChargeBytes: bytes, Source: source, Observation: p.Stamp}, ledger: next, candidate: candidate}, nil
}
func (i *StoragePlanIterator) options(ctx context.Context, r StorageRoleSelection, bytes uint64, base inv.Ledger, nodes []string, primary bool) ([]storagePlanOption, error) {
	ids, err := planRoleMembers(i.req.Inventory, r)
	if err != nil {
		return nil, err
	}
	if r.Role == storageRoleRoot && i.req.CloneMode == "linked" && r.Kind == storageSelectorSet && len(ids) > 1 {
		return nil, planError(StoragePlanConfiguration, "linked-only root requires singleton set")
	}
	byBacking := map[string][]storagePlanOption{}
	for _, id := range ids {
		for _, node := range nodes {
			if err := i.consume(ctx); err != nil {
				return nil, err
			}
			option, eligible, err := i.optionFor(r, bytes, base, node, id, primary)
			if err != nil {
				return nil, err
			}
			if !eligible {
				continue
			}

			key := option.target.CapacityKey
			byBacking[key] = append(byBacking[key], option)
		}
	}
	var candidates []rank.EligibleCandidate
	keys := make([]string, 0, len(byBacking))
	for key := range byBacking {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		options := byBacking[key]
		representative := options[0].candidate
		// Conservative residual across alternatives freezes one node-independent weight.
		for optionIndex := 1; optionIndex < len(options); optionIndex++ {
			o := &options[optionIndex]
			representative.Member.OutstandingBytes = max(representative.Member.OutstandingBytes, o.candidate.Member.OutstandingBytes)
			representative.Member.AllocationBytes = max(representative.Member.AllocationBytes, o.candidate.Member.AllocationBytes)
			representative.Domain.OutstandingBytes = max(representative.Domain.OutstandingBytes, o.candidate.Domain.OutstandingBytes)
			representative.Domain.AllocationBytes = max(representative.Domain.AllocationBytes, o.candidate.Domain.AllocationBytes)
		}
		if _, err := rank.ProjectBudget(representative.Member); err != nil {
			continue
		}
		if representative.DomainKey != "" {
			if _, err := rank.ProjectBudget(representative.Domain); err != nil {
				continue
			}
		}
		representative.BackingKey = key
		candidates = append(candidates, representative)
	}
	policy := rank.Policy{Name: "spread", Version: 1}
	if r.Set != nil {
		policy = rank.Policy{Name: r.Set.Strategy.Name, Version: r.Set.Strategy.Version}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	ranked, err := rank.Rank(rank.RequestSnapshot{Policy: policy, Namespace: i.req.Namespace, AllocationKey: i.req.AllocationKey, AllocationGroup: r.Role, RequestedBytes: bytes, Seed: i.req.Seed, SeedSet: true}, candidates)
	if err != nil {
		return nil, planError(StoragePlanConfiguration, "rank: %v", err)
	}
	var out []storagePlanOption
	for rangeIndex637 := range ranked {
		options := byBacking[ranked[rangeIndex637].Candidate.BackingKey]
		for _, node := range nodes {
			for optionIndex := range options {
				if options[optionIndex].target.Node == node {
					o := options[optionIndex]
					o.candidate = ranked[rangeIndex637].Candidate
					o.ranking = ranked[rangeIndex637]
					out = append(out, o)
				}
			}
		}
	}
	return out, nil
}
func (i *StoragePlanIterator) isoTarget(node, root string) (string, error) {
	id := i.req.OriginalISOStorage
	if i.req.ISOFollowRoot && (id == "" || id == "local") {
		d, ok := i.req.Inventory.Definition(root)
		if ok && d.IsShared() && slices.Contains(strings.Split(d.Content, ","), storageRoleISO) {
			id = root
		}
	}
	if id == "" {
		return "", fmt.Errorf("ISO target is not configured")
	}
	d, ok := i.req.Inventory.Definition(id)
	if !ok || !planContent(d, storageRoleISO) {
		return "", fmt.Errorf("ISO target lacks supported iso backend/content")
	}
	if (i.req.RequireSharedISO || len(i.haNodes()) > 0) && !d.IsShared() {
		return "", fmt.Errorf("ISO target is not shared")
	}
	if err := i.accessible(node, id); err != nil {
		return "", err
	}
	for _, n := range i.haNodes() {
		if err := i.accessible(n, id); err != nil {
			return "", err
		}
	}
	return id, nil
}
func (i *StoragePlanIterator) isoLedger(l inv.Ledger, node, root string) (inv.Ledger, error) {
	id, err := i.isoTarget(node, root)
	if err != nil {
		return l, err
	}
	return l.WithPlanned(i.req.Inventory, inv.Charge{ID: storageRoleISO, Role: storageRoleISO, Node: node, StorageID: id, Bytes: i.req.ISOBytes, Limits: i.req.ExtraLimits})
}
func (i *StoragePlanIterator) makePlan(root storagePlanOption, ep *storagePlanOption) *StorageAllocationPlan {
	l := root.ledger
	targets := []StoragePlanTarget{root.target}
	rankings := []rank.RankedCandidate{root.ranking}
	if ep != nil {
		l = ep.ledger
		targets = append(targets, ep.target)
		if !i.req.Selection.BundleVMDisks {
			rankings = append(rankings, ep.ranking)
		}
	}
	records := l.Records()
	for index := range records {
		c := &records[index]
		if c.Charge.Role == storageRoleISO {
			p, _ := i.req.Inventory.Pair(c.Charge.Node, c.Charge.StorageID)
			targets = append(targets, StoragePlanTarget{Role: storageRoleISO, Node: c.Charge.Node, StorageID: c.Charge.StorageID, BackingKey: p.BackingKey, CapacityKey: p.CapacityKey, DomainKey: c.DomainKey, Mechanism: "upload", VirtualBytes: c.Charge.Bytes, ChargeBytes: c.Charge.Bytes, Observation: p.Stamp})
		}
	}
	strategies := map[string]rank.Policy{}
	for _, role := range []*StorageRoleSelection{i.req.Selection.Root, i.req.Selection.Ephemeral, i.req.Selection.Persistent} {
		if role != nil {
			p := rank.Policy{Name: "spread", Version: 1}
			if role.Set != nil {
				p = rank.Policy{Name: role.Set.Strategy.Name, Version: role.Set.Strategy.Version}
			}
			strategies[role.Role] = p
		}
	}
	definitions := map[string]pve.StorageInfo{}
	domains := map[string][]string{}
	for index := range targets {
		target := &targets[index]
		ids := []string{target.StorageID}
		if target.DomainKey != "" {
			if domain, ok := i.req.Inventory.Domain(target.DomainKey); ok {
				domains[target.DomainKey] = slices.Clone(domain.Members)
				ids = append(ids, domain.Members...)
			}
		}
		if target.Source != nil {
			ids = append(ids, target.Source.StorageID)
			for _, aux := range target.Source.AuxiliaryVolumes {
				ids = append(ids, aux.StorageID)
			}
		}
		for _, id := range ids {
			if d, ok := i.req.Inventory.Definition(id); ok {
				definitions[id] = d
			}
		}
	}
	return &StorageAllocationPlan{Version: 1, Namespace: i.req.Namespace, AllocationKey: i.req.AllocationKey, PolicyFingerprint: i.fingerprint, AZ: i.req.Groups[i.group].AZ, Node: root.target.Node, Seed: i.req.Seed, HANodes: i.haNodes(), Targets: targets, Charges: storagePlanExecutionCharges(l.Records(), targets), Rankings: rankings, Strategies: strategies, Definitions: definitions, CapacityDomains: domains, Rejections: slices.Clone(i.rejections)}
}

func (i *StoragePlanIterator) haNodes() []string {
	nodes := slices.Clone(i.req.HANodes)
	nodes = append(nodes, i.req.Groups[i.group].HANodes...)
	slices.Sort(nodes)
	return slices.Compact(nodes)
}

// storagePlanExecutionCharges preserves ranking's total debit while separating
// creation from later expansion. A refreshed status can then reflect the base
// allocation without dropping the outstanding growth reservation.
func storagePlanExecutionCharges(records []inv.ChargeRecord, targets []StoragePlanTarget) []inv.ChargeRecord {
	var root *StoragePlanTarget
	for index := range targets {
		if targets[index].Role == storageRoleRoot {
			root = &targets[index]
			break
		}
	}
	if root == nil || root.Source == nil {
		return records
	}
	out := make([]inv.ChargeRecord, 0, len(records)+3)
	for rangeIndex760 := range records {
		if records[rangeIndex760].Charge.Role != storageRoleRoot {
			out = append(out, records[rangeIndex760])
			continue
		}
		base := ((root.Source.VirtualBytes-1)/(1<<30) + 1) * (1 << 30)
		portions := []struct {
			id    string
			bytes uint64
		}{{"root_base", base}, {"root_growth", root.VirtualBytes - base}, {"root_auxiliary", root.Source.AuxiliaryBytes}, {"root_scratch", root.Source.ScratchBytes}}
		for _, portion := range portions {
			if portion.bytes == 0 {
				continue
			}
			cloned := records[rangeIndex760]
			cloned.Charge.ID = portion.id
			cloned.Charge.Bytes = portion.bytes
			out = append(out, cloned)
		}
	}
	return out
}

func (i *StoragePlanIterator) optionFor(r StorageRoleSelection, bytes uint64, base inv.Ledger, node, id string, primary bool) (storagePlanOption, bool, error) {
	var err error
	if err = i.accessConstraints(node, id); err != nil {
		i.rejectCandidate(r.Role, err)
		return storagePlanOption{}, false, nil
	}
	ledger := base
	if primary && i.req.ISOBytes > 0 {
		ledger, err = i.isoLedger(ledger, node, id)
		if err != nil {
			i.rejectCandidate(r.Role, err)
			return storagePlanOption{}, false, nil
		}
	}
	virtual, charge, mechanism := bytes, bytes, "allocate"
	var source *StorageRootSource
	if r.Role == storageRoleRoot {
		source, mechanism, err = i.sourceFor(node, id)
		if err != nil {
			i.rejectCandidate(r.Role, err)
			return storagePlanOption{}, false, nil
		}
		virtual = max(bytes, source.VirtualBytes)
		virtual, err = inv.RoundBytes(virtual, 1<<30)
		if err != nil {
			return storagePlanOption{}, false, err
		}
		if virtual > math.MaxUint64-source.ScratchBytes {
			return storagePlanOption{}, false, planError(StoragePlanConfiguration, "root charge overflow")
		}
		charge = virtual + source.ScratchBytes
		if charge > math.MaxUint64-source.AuxiliaryBytes {
			return storagePlanOption{}, false, planError(StoragePlanConfiguration, "auxiliary charge overflow")
		}
		charge += source.AuxiliaryBytes
	}
	option, err := i.addTarget(ledger, r.Role, node, id, virtual, charge, mechanism, source, r)
	if err != nil {
		i.rejectCandidate(r.Role, err)
		return storagePlanOption{}, false, nil
	}
	// Bundle ranking includes both disks, though the ledger keeps separate steps.
	if primary && i.req.Selection.BundleVMDisks && i.req.Selection.Ephemeral != nil {
		limits, err := i.limits(*i.req.Selection.Ephemeral)
		if err != nil {
			return storagePlanOption{}, false, err
		}
		combined, err := inv.CombineLimits(limits, option.ledger.Records()[len(option.ledger.Records())-1].Charge.Limits)
		if err != nil {
			return storagePlanOption{}, false, err
		}
		if charge > math.MaxUint64-i.req.EphemeralBytes {
			return storagePlanOption{}, false, planError(StoragePlanConfiguration, "bundle charge overflow")
		}
		option.candidate, err = ledger.Candidate(i.req.Inventory, node, id, charge+i.req.EphemeralBytes, combined)
		if err != nil {
			i.rejectCandidate(r.Role, err)
			return storagePlanOption{}, false, nil
		}
	}
	return option, true, nil
}

func (i *StoragePlanIterator) sourceAuxiliaryAccessible(node string, volumes []StorageExistingVolume) bool {
	auxOK := true
	for _, aux := range volumes {
		ad, ok := i.req.Inventory.Definition(aux.StorageID)
		if !ok || !planContent(ad, "images") || !ad.IsShared() && aux.Node != node || i.accessible(node, aux.StorageID) != nil {
			auxOK = false
			break
		}
	}
	return auxOK
}

func (i *StoragePlanIterator) rejectCandidate(role string, err error) {
	i.rejections = append(i.rejections, err.Error())
	if i.req.OnCandidateRejected != nil {
		i.req.OnCandidateRejected(role)
	}
}
