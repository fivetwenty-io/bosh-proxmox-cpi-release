package handlers

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// storageLister is the minimal interface used by resolveStorageTier to list
// cluster storages. Production callers pass deps.PVE.ClusterStorage(); tests
// substitute a fake via the Deps wiring.
type storageLister interface {
	ListStorage(ctx context.Context, params *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error)
}

// resolveStorageTier returns the storage pool name that matches the named tier's
// criteria. It lists all cluster storages, filters by config.StorageTiers[tier]
// (Types allowlist AND Shared predicate when set), then returns the
// lexicographically-first match for determinism.
//
// Inputs and failure modes:
//   - tier absent from cfg.StorageTiers → non-retriable CloudError naming the tier.
//     The live storage list is NOT queried in this case.
//   - list/API error → wrapped error preserving the retriable classification from
//     pve.WrapError (transport faults are retriable; auth/4xx errors are not).
//   - nil or empty list response → treated as zero matches.
//   - zero matches after filtering → non-retriable CloudError naming the tier.
//   - entry with malformed JSON → skipped (same behavior as StorageInfoCache.refresh).
//   - ctx cancelled before or during list → wrapped context error returned.
func resolveStorageTier(ctx context.Context, lister storageLister, cfg *config.CPIConfig, tier string) (string, error) {
	criteria, ok := cfg.StorageTiers[tier]
	if !ok {
		return "", cpierrors.Cloud(
			"storage_tier %q: unknown tier (not declared in cpi config storage_tiers map)",
			tier,
		)
	}

	resp, err := lister.ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil {
		return "", cpierrors.Wrap(pve.WrapError(err), "storage_tier "+tier+": list cluster storage")
	}
	if resp == nil {
		return "", cpierrors.Cloud("storage_tier %q: no matching storage found (0 cluster storages returned)", tier)
	}

	var matches []string
	for _, raw := range *resp {
		info, perr := parseTierStorageEntry(raw)
		if perr != nil {
			continue
		}
		if storageTierMatches(info, criteria) {
			matches = append(matches, info.Name)
		}
	}

	if len(matches) == 0 {
		return "", cpierrors.Cloud(
			"storage_tier %q: no storage matched criteria (types=%v shared=%v)",
			tier, criteria.Types, criteria.Shared,
		)
	}

	sort.Strings(matches)
	return matches[0], nil
}

// storageTierMatches reports whether info satisfies criteria.
// Matching rules:
//   - If criteria.Types is non-empty, info.Type must be in the list (case-insensitive).
//   - If criteria.Shared is non-nil, info.IsShared() must equal *criteria.Shared.
//   - Both predicates must hold when both are set.
func storageTierMatches(info pve.StorageInfo, criteria config.StorageTierCriteria) bool {
	if len(criteria.Types) > 0 {
		typeOK := false
		for _, allowed := range criteria.Types {
			if strings.EqualFold(info.Type, allowed) {
				typeOK = true
				break
			}
		}
		if !typeOK {
			return false
		}
	}
	if criteria.Shared != nil {
		if info.IsShared() != *criteria.Shared {
			return false
		}
	}
	return true
}

// parseTierStorageEntry decodes a json.RawMessage from the cluster storage list
// into a pve.StorageInfo. Only the fields needed for tier matching are decoded.
// Uses the same subset as StorageInfoCache.refresh and handlerPolicyDeps.StorageInfo.
func parseTierStorageEntry(raw json.RawMessage) (pve.StorageInfo, error) {
	var v struct {
		Storage string `json:"storage"`
		Type    string `json:"type"`
		Shared  *int   `json:"shared,omitempty"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return pve.StorageInfo{}, err
	}
	if v.Storage == "" {
		return pve.StorageInfo{}, cpierrors.Cloud("storage_tier: entry missing storage name")
	}
	info := pve.StorageInfo{
		Name: v.Storage,
		Type: v.Type,
	}
	if v.Shared != nil && *v.Shared != 0 {
		info.Shared = true
	}
	return info, nil
}
