package handlers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	rank "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
)

// StoragePlanDiagnosticRequest uses the CPI's method and arguments envelope.
// Request arguments are never copied into diagnostic output.
type StoragePlanDiagnosticRequest struct {
	Method    string            `json:"method"`
	Arguments []json.RawMessage `json:"arguments"`
}

// StorageSelectorDiagnostic explains one resolved selector and its constraints.
type StorageSelectorDiagnostic struct {
	Role, Kind, Value, Layer, Property, Boundary string
	Encrypted                                    bool
	ReserveMB                                    int64
	CeilingPct                                   *int
}

// StorageCapacityDiagnostic reports an observed capacity and its age.
type StorageCapacityDiagnostic struct {
	Pair            inv.Pair
	Domain          string
	AgeMilliseconds int64
}

// StorageCandidateDiagnostic ties a rank to its role and candidate node.
type StorageCandidateDiagnostic struct {
	Role, Node string
	Ranking    rank.RankedCandidate
}

// StorageJournalDiagnostic exposes a bounded retained-record summary.
type StorageJournalDiagnostic struct{ AllocationID, Kind, State, CID string }

// StoragePlanDiagnostic contains observations only: it is not a reservation or
// permission to reuse its sampled allocation identity for a later create call.
type StoragePlanDiagnostic struct {
	CandidateRanks   []StorageCandidateDiagnostic `json:"candidate_ranks,omitempty"`
	Method           string                       `json:"method"`
	ObservationOnly  bool                         `json:"observation_only"`
	ObservedAt       time.Time                    `json:"observed_at"`
	Selectors        []StorageSelectorDiagnostic  `json:"selectors"`
	FrozenMembership map[string][]string          `json:"frozen_membership"`
	Capacities       []StorageCapacityDiagnostic  `json:"capacities"`
	Domains          []inv.Domain                 `json:"domains"`
	Node             string                       `json:"node,omitempty"`
	AllocationKey    string                       `json:"allocation_key,omitempty"`
	Seed             string                       `json:"seed,omitempty"`
	Targets          []StoragePlanTarget          `json:"targets,omitempty"`
	Rankings         []rank.RankedCandidate       `json:"rankings,omitempty"`
	Strategies       map[string]rank.Policy       `json:"strategies,omitempty"`
	Rejections       []string                     `json:"rejections,omitempty"`
	Journal          []StorageJournalDiagnostic   `json:"journal,omitempty"`
	Findings         []string                     `json:"findings,omitempty"`
}

// ObserveStoragePlanDiagnostics invokes production planning under a client that
// rejects every mutation. It never allocates a VMID or acquires journal ownership.
func ObserveStoragePlanDiagnostics(ctx context.Context, deps Deps, request StoragePlanDiagnosticRequest) (*StoragePlanDiagnostic, error) {
	if ctx == nil || deps.Config == nil || deps.PVE == nil {
		return nil, errors.New("storage-plan: context, configuration and client are required")
	}
	if request.Method != "create_disk" && request.Method != "create_vm" {
		return nil, errors.New("storage-plan: method must be create_disk or create_vm")
	}
	deny := errors.New("storage-plan: mutation prohibited")
	guard, err := NewManagedAllocationGuard(deps.PVE, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "", deny },
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return deny },
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error { return deny },
	})
	if err != nil {
		return nil, errors.New("storage-plan: read-only client unavailable")
	}
	deps.PVE = guard.Client()
	deps.Logger = log.NewNopLogger()
	var selection *StoragePlacementSelection
	var snapshot *inv.Snapshot
	var plan *StorageAllocationPlan
	var iterator *StoragePlanIterator
	if request.Method == "create_disk" {
		selection, snapshot, plan, iterator, err = observeDiskPlan(ctx, deps, request.Arguments)
	} else {
		selection, snapshot, plan, iterator, err = observeVMPlan(ctx, deps, request.Arguments)
	}
	if guard.Err() != nil {
		return nil, deny
	}
	out := &StoragePlanDiagnostic{Method: request.Method, ObservationOnly: true, ObservedAt: time.Now().UTC(), FrozenMembership: map[string][]string{}}
	if err != nil {
		kind := storageDiagnosticRejectionKind(err)
		out.Rejections = append(out.Rejections, "planning rejected: "+kind)
	}
	if iterator != nil {
		out.Rejections = append(out.Rejections, iterator.rejections...)
		out.Seed = hex.EncodeToString(iterator.req.Seed[:])
		out.AllocationKey = iterator.req.AllocationKey
		for _, options := range [][]storagePlanOption{iterator.roots, iterator.continuations} {
			for optionIndex := range options {
				option := options[optionIndex]
				out.CandidateRanks = append(out.CandidateRanks, StorageCandidateDiagnostic{option.target.Role, option.target.Node, option.ranking})
			}
		}
	}
	if selection != nil {
		collectStorageDiagnostics(out, selection, snapshot, plan)
	}
	if plan != nil {
		out.Node = plan.Node
		out.AllocationKey = plan.AllocationKey
		out.Seed = hex.EncodeToString(plan.Seed[:])
		out.Targets = plan.Targets
		out.Rankings = plan.Rankings
		out.Strategies = plan.Strategies
		// Planner rejection text contains storage identities and fixed reason codes,
		// never API bodies or cloud properties.
		if iterator == nil {
			out.Rejections = append(out.Rejections, plan.Rejections...)
		}
	}
	if snapshot != nil {
		observeDiagnosticJournal(ctx, deps, snapshot.Nodes(), out)
	}
	return out, nil
}

func observeDiskPlan(ctx context.Context, deps Deps, args []json.RawMessage) (*StoragePlacementSelection, *inv.Snapshot, *StorageAllocationPlan, *StoragePlanIterator, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, nil, nil, nil, errors.New("invalid disk arguments")
	}
	var size int
	var cp createDiskCloudProperties
	var props map[string]any
	var hint string
	if json.Unmarshal(args[0], &size) != nil || json.Unmarshal(args[1], &cp) != nil || json.Unmarshal(args[1], &props) != nil {
		return nil, nil, nil, nil, errors.New("invalid disk arguments")
	}
	if len(args) == 3 && json.Unmarshal(args[2], &hint) != nil {
		return nil, nil, nil, nil, errors.New("invalid disk hint")
	}
	selection, err := ResolveStoragePlacementSelectors(deps.Config, "create_disk", props, false)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	resolver, err := newLayeredResolver(props, deps.Config)
	if err != nil {
		return selection, nil, nil, nil, err
	}
	m, err := prepareManagedDisk(ctx, deps, selection, size, cp, hint, resolver)
	if m == nil {
		return selection, nil, nil, nil, err
	}
	return selection, m.inventory, m.plan, m.iterator, err
}
func observeVMPlan(ctx context.Context, deps Deps, args []json.RawMessage) (*StoragePlacementSelection, *inv.Snapshot, *StorageAllocationPlan, *StoragePlanIterator, error) {
	if len(args) != 6 {
		return nil, nil, nil, nil, errors.New("invalid VM arguments")
	}
	parsed, err := parseCreateVMArgs(args)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var props map[string]any
	if err := json.Unmarshal(args[2], &props); err != nil {
		return nil, nil, nil, nil, err
	}
	selection, err := ResolveStoragePlacementSelectors(deps.Config, "create_vm", props, parsed.cloudProps.EphemeralDiskSizeMB > 0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	m, err := prepareManagedVMPlan(ctx, deps, parsed, selection)
	if m == nil {
		return selection, nil, nil, nil, err
	}
	return selection, m.inventory, m.plan, m.iterator, err
}
func collectStorageDiagnostics(out *StoragePlanDiagnostic, s *StoragePlacementSelection, snapshot *inv.Snapshot, plan *StorageAllocationPlan) {
	ids := map[string]bool{}
	if plan != nil {
		for targetIndex := range plan.Targets {
			target := plan.Targets[targetIndex]
			ids[target.StorageID] = true
			if target.Source != nil {
				ids[target.Source.StorageID] = true
				for _, aux := range target.Source.AuxiliaryVolumes {
					ids[aux.StorageID] = true
				}
			}
		}
	}
	collectStorageSelectorDiagnostics(out, s, snapshot, ids)
	if snapshot == nil {
		return
	}
	names := make([]string, 0, len(s.Policy.StorageSets))
	for name := range s.Policy.StorageSets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if members, ok := snapshot.Members(name); ok {
			out.FrozenMembership[name] = members
			for _, id := range members {
				ids[id] = true
			}
		}
	}
	names = names[:0]
	for name := range s.Policy.StorageCapacityDomains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if d, ok := snapshot.Domain(name); ok {
			out.Domains = append(out.Domains, d)
			for _, id := range d.Members {
				ids[id] = true
			}
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, node := range snapshot.Nodes() {
		for _, id := range ordered {
			if pair, ok := snapshot.Pair(node, id); ok {
				out.Capacities = append(out.Capacities, StorageCapacityDiagnostic{pair, snapshot.DomainForStorage(id), max(0, out.ObservedAt.Sub(pair.Stamp.StartedAt).Milliseconds())})
			}
		}
	}
}
func observeDiagnosticJournal(ctx context.Context, deps Deps, nodes []string, out *StoragePlanDiagnostic) {
	journal, err := openStorageAllocationJournal(ctx, deps, nodes)
	continuityVerified := err == nil
	if err != nil {
		status, inspectErr := aj.InspectEnrollment(deps.Config.StorageAllocationJournalDir, deps.Config.StoragePlacementNamespace)
		if inspectErr != nil {
			out.Findings = append(out.Findings, "private journal unavailable")
			return
		}
		journal, err = aj.Open(deps.Config.StorageAllocationJournalDir, deps.Config.StoragePlacementNamespace, status.Enrollment.ClusterID)
		if err != nil {
			out.Findings = append(out.Findings, "private journal unavailable")
			return
		}
		out.Findings = append(out.Findings, "journal enrollment not checked against live cluster; retained CIDs are records, not verified ownership")
	}
	defer func() {
		if journal.Close() != nil {
			out.Findings = append(out.Findings, "private journal close failed")
		}
	}()
	records, err := journal.List()
	if err != nil {
		out.Findings = append(out.Findings, "private journal records unreadable")
		return
	}
	for rIndex := range records {
		r := records[rIndex]
		out.Journal = append(out.Journal, StorageJournalDiagnostic{r.ID, r.Kind, string(r.State), r.CID})
	}
	probe := "storage-plan-generation-index-health-probe"
	for rIndex := range records {
		r := records[rIndex]
		if r.Kind == "vm" {
			probe = r.AgentID
			break
		}
	}
	if _, _, err := journal.InspectVMContext(ctx, probe); err != nil {
		out.Findings = append(out.Findings, "generation index invalid or unavailable; record display does not establish allocation authority")
		return
	}
	if !continuityVerified {
		return
	}
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		out.Findings = append(out.Findings, "historical allocation observations unavailable")
		return
	}
	out.Findings = append(out.Findings, fmt.Sprintf("historical audit complete=%t; conflicts=%d; issues=%d", report.Complete, len(report.Conflicts), len(report.Issues)))
}

func collectStorageSelectorDiagnostics(out *StoragePlanDiagnostic, s *StoragePlacementSelection, snapshot *inv.Snapshot, ids map[string]bool) {
	out.Strategies = map[string]rank.Policy{}
	for _, r := range []*StorageRoleSelection{s.Root, s.Ephemeral, s.Persistent} {
		if r == nil {
			continue
		}
		policy := rank.Policy{Name: "spread", Version: 1}
		if r.Set != nil {
			policy = rank.Policy{Name: r.Set.Strategy.Name, Version: r.Set.Strategy.Version}
		}
		out.Strategies[r.Role] = policy
		out.Selectors = append(out.Selectors, StorageSelectorDiagnostic{r.Role, r.Kind, r.Value, r.Source.Layer, r.Source.Property, r.BoundaryName, r.Encrypted, r.ReserveMB, r.CeilingPct})
		if snapshot != nil {
			if r.Kind == storageSelectorPool {
				ids[r.Value] = true
			}
			if r.Kind == storageSelectorTier {
				if id, ok := snapshot.TierStorage(r.Value); ok {
					ids[id] = true
				}
			}
		}
	}

}

func storageDiagnosticRejectionKind(err error) string {
	var policy *inv.PolicyError
	if errors.As(err, &policy) {
		switch policy.Code {
		case inv.PolicyMissingStorage, inv.PolicyBackingAliases, inv.PolicyOverlappingSets:
			return policy.Code
		}
	}
	var plan *StoragePlanError
	if errors.As(err, &plan) {
		return string(plan.Kind)
	}
	return "configuration or observation"
}
