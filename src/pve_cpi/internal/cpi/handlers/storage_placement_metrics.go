package handlers

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// StoragePlacementMetrics uses the existing opt-in meter and its delta export.
// Every attribute comes from a configured set or a fixed role/strategy enum.
// Allocation identities, storage IDs, regexes and backend errors are excluded.
type StoragePlacementMetrics struct{ allocations, fallbacks, rejections, candidateRejections, reconciliations metric.Int64Counter }

// NewStoragePlacementMetrics registers bounded placement and reconciliation counters.
func NewStoragePlacementMetrics(meter metric.Meter) (*StoragePlacementMetrics, error) {
	if meter == nil {
		return nil, nil
	}
	m := &StoragePlacementMetrics{}
	var err error
	m.allocations, err = meter.Int64Counter("cpi.storage.allocations", metric.WithDescription("Completed storage allocations by configured role policy"))
	if err != nil {
		return nil, fmt.Errorf("storage allocation counter: %w", err)
	}
	m.fallbacks, err = meter.Int64Counter("cpi.storage.fallbacks", metric.WithDescription("Verified transitions to another storage allocation attempt"))
	if err != nil {
		return nil, fmt.Errorf("storage fallback counter: %w", err)
	}
	m.rejections, err = meter.Int64Counter("cpi.storage.rejections", metric.WithDescription("Storage allocation requests ending without a returned CID"))
	if err != nil {
		return nil, fmt.Errorf("storage rejection counter: %w", err)
	}
	m.candidateRejections, err = meter.Int64Counter("cpi.storage.candidate_rejections", metric.WithDescription("Storage candidates excluded by feasibility checks"))
	if err != nil {
		return nil, fmt.Errorf("storage candidate rejection counter: %w", err)
	}
	m.reconciliations, err = meter.Int64Counter("cpi.storage.reconciliations", metric.WithDescription("Explicit allocation reconciliation outcomes"))
	if err != nil {
		return nil, fmt.Errorf("storage reconciliation counter: %w", err)
	}
	return m, nil
}

func (d Deps) recordStoragePlacement(ctx context.Context, selection *StoragePlacementSelection, event string) {
	if d.StorageMetrics == nil || selection == nil || selection.Policy == nil {
		return
	}
	var counter metric.Int64Counter
	switch event {
	case "allocation":
		counter = d.StorageMetrics.allocations
	case "fallback":
		counter = d.StorageMetrics.fallbacks
	case "rejection":
		counter = d.StorageMetrics.rejections
	case "candidate_rejection":
		counter = d.StorageMetrics.candidateRejections
	default:
		return
	}
	if counter == nil {
		return
	}
	for _, entry := range []struct {
		role      string
		selection *StorageRoleSelection
	}{{storageRoleRoot, selection.Root}, {storageRoleEphemeral, selection.Ephemeral}, {storageRolePersistent, selection.Persistent}} {
		r := entry.selection
		if r == nil {
			continue
		}
		setName := r.SetName
		if setName == "" {
			setName = r.BoundaryName
		}
		set, configured := selection.Policy.StorageSets[setName]
		if !configured {
			continue
		}
		strategy := set.Strategy.Name
		switch strategy {
		case "spread", "weighted_free_space", "least_utilized":
		default:
			strategy = "invalid"
		}
		version := set.Strategy.Version
		if version != 1 {
			version = 0
		}
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String("storage.set", setName), attribute.String("storage.role", entry.role), attribute.String("storage.strategy", strategy), attribute.Int("storage.strategy.version", version)))
	}
}

// Reconciliation uses a fixed outcome vocabulary even after set removal.
func (d Deps) recordStorageReconciliation(ctx context.Context, outcome string) {
	if d.StorageMetrics == nil || d.StorageMetrics.reconciliations == nil {
		return
	}
	switch outcome {
	case "adopted", "cleaned", "rejected", "required":
	default:
		return
	}
	d.StorageMetrics.reconciliations.Add(ctx, 1, metric.WithAttributes(attribute.String("storage.reconciliation.outcome", outcome)))
}

// storageCandidateRejectionObserver records only the rejected role's configured
// policy. It never accepts storage identities or backend reasons as dimensions.
func (d Deps) storageCandidateRejectionObserver(ctx context.Context, selection *StoragePlacementSelection) func(string) {
	if d.StorageMetrics == nil || selection == nil {
		return nil
	}
	return func(role string) {
		scoped := *selection
		scoped.Root = nil
		scoped.Ephemeral = nil
		scoped.Persistent = nil
		switch role {
		case storageRoleRoot:
			scoped.Root = selection.Root
		case storageRoleEphemeral:
			scoped.Ephemeral = selection.Ephemeral
		case storageRolePersistent:
			scoped.Persistent = selection.Persistent
		default:
			return
		}
		d.recordStoragePlacement(ctx, &scoped, "candidate_rejection")
	}
}
