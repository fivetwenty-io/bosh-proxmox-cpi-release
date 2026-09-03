package handlers

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ResolveEncrypted returns the effective encrypted bool following the two-level
// precedence: per-call callLevel > global. Both nil → false (byte-identical, no filter).
// Exported so tests in the _test package can verify the resolution contract directly.
func ResolveEncrypted(global, callLevel *bool) bool {
	if callLevel != nil {
		return *callLevel
	}
	if global != nil {
		return *global
	}
	return false
}

// selectEncryptedTier picks the lexicographically-first tier name from cfg.StorageTiers
// where criteria.Encrypted is non-nil and *true. Returns ("", false) when none exist.
func selectEncryptedTier(cfg *config.CPIConfig) (string, bool) {
	var candidates []string
	for name, c := range cfg.StorageTiers {
		if c.Encrypted != nil && *c.Encrypted {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Strings(candidates)
	return candidates[0], true
}

// resolveEncryptedPool auto-selects a pool when encrypted=true but no explicit
// tier or pool was named by the caller. It picks the lex-first tier from
// cfg.StorageTiers where Encrypted is *true and runs the normal resolveStorageTier
// path (Types/Shared predicates + live query). Also returns the selected tier name
// so the caller can log a warning.
//
// Returns a non-retriable CloudError when:
//   - no tier in config is marked Encrypted:*true (op names the calling operation)
//   - the selected tier yields no matching pool (propagated from resolveStorageTier)
func resolveEncryptedPool(ctx context.Context, lister storageLister, cfg *config.CPIConfig, op string) (pool, tier string, err error) {
	t, ok := selectEncryptedTier(cfg)
	if !ok {
		return "", "", cpierrors.Cloud(
			"%s: encrypted=true but no storage tier is marked encrypted in cpi config"+
				" storage_tiers (add a tier with encrypted: true)",
			op,
		)
	}
	p, e := resolveStorageTier(ctx, lister, cfg, t, true)
	if e != nil {
		return "", "", e
	}
	return p, t, nil
}

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
// encrypted controls the §7.49 encrypted-storage filter:
//   - false (default) — the Encrypted field on the tier criteria is ignored; all
//     pools that satisfy Types/Shared are eligible (byte-identical to pre-§7.49).
//   - true — only pools from tiers where criteria.Encrypted is *true are eligible.
//     If the named tier is not marked Encrypted:*true → non-retriable CloudError.
//
// Inputs and failure modes:
//   - tier absent from cfg.StorageTiers → non-retriable CloudError naming the tier.
//     The live storage list is NOT queried in this case.
//   - encrypted=true but tier.Encrypted is nil or *false → non-retriable CloudError.
//   - list/API error → wrapped error preserving the retriable classification from
//     pve.WrapError (transport faults are retriable; auth/4xx errors are not).
//   - nil or empty list response → treated as zero matches.
//   - zero matches after filtering → non-retriable CloudError naming the tier.
//   - entry with malformed JSON → skipped (same behavior as StorageInfoCache.refresh).
//   - ctx cancelled before or during list → wrapped context error returned.
func resolveStorageTier(ctx context.Context, lister storageLister, cfg *config.CPIConfig, tier string, encrypted bool) (string, error) {
	criteria, ok := cfg.StorageTiers[tier]
	if !ok {
		return "", cpierrors.Cloud(
			"storage_tier %q: unknown tier (not declared in cpi config storage_tiers map)",
			tier,
		)
	}

	// Encrypted contradiction check: if the caller requires encrypted storage but
	// the named tier is not marked encrypted, fail non-retriably before the live
	// query so the operator gets a clear message.
	if encrypted {
		if criteria.Encrypted == nil || !*criteria.Encrypted {
			return "", cpierrors.Cloud(
				"storage_tier %q: encrypted=true is required but this tier is not marked"+
					" encrypted in cpi config storage_tiers (set encrypted: true on the tier criteria)",
				tier,
			)
		}
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
			"storage_tier %q: no storage matched criteria (types=%v shared=%v encrypted=%v)",
			tier, criteria.Types, criteria.Shared, criteria.Encrypted,
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
		Storage string             `json:"storage"`
		Type    string             `json:"type"`
		Shared  *sdkclient.PVEBool `json:"shared,omitempty"`
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
	if v.Shared != nil && v.Shared.Bool() {
		info.Shared = true
	}
	return info, nil
}
