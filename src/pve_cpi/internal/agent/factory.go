package agent

import (
	"context"
	"fmt"
	"strings"

	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// NewAgent returns the Agent implementation selected by cfg.AgentMode.
//
//   - "cloudinit" → ConfigDrive using pveClient; storage is cfg.ISOStorage when
//     non-empty, else cfg.StemcellStorage, else cfg.VMStorage.
//   - "noagent"   → NoAgent (no-op).
//   - "auto"      → treated as "cloudinit" by callers before NewAgent is called
//     (main.go rewrites AgentMode to "cloudinit" for the primary boot agent).
//   - any other   → *errors.Error of type NotSupported.
//
// cfg and logger must not be nil. pveClient may be nil only when
// cfg.AgentMode == "noagent".
func NewAgent(cfg *config.CPIConfig, pveClient pve.Client, logger *log.Logger) (Agent, error) {
	if cfg == nil {
		return nil, cpierrors.Cloud("agent.NewAgent: cfg must not be nil")
	}
	if logger == nil {
		return nil, cpierrors.Cloud("agent.NewAgent: logger must not be nil")
	}

	switch cfg.AgentMode {
	case config.AgentModeCloudInit:
		if pveClient == nil {
			return nil, cpierrors.Cloud("agent.NewAgent: pveClient required for cloudinit mode")
		}
		// ConfigDrive ISO must live on a dir/nfs/cifs storage that supports
		// content type `iso` — block storages (lvm/lvmthin/zfspool) reject
		// upload-to-storage entirely. ISOStorage defaults to "local".
		isoStorage := cfg.ISOStorage
		if isoStorage == "" {
			isoStorage = cfg.StemcellStorage
		}
		if isoStorage == "" {
			isoStorage = cfg.VMStorage
		}
		if isoStorage == "" {
			return nil, cpierrors.Cloud("agent.NewAgent: cloudinit mode requires ISOStorage, StemcellStorage, or VMStorage to be set")
		}
		return NewConfigDrive(pveClient, isoStorage, logger), nil

	case config.AgentModeNoAgent:
		if cfg.ISOStorage != "" {
			logger.Warn("agent.NewAgent: agent_mode=noagent ignores iso_storage",
				log.String("iso_storage", cfg.ISOStorage))
		}
		return NewNoAgent(logger), nil

	default:
		return nil, cpierrors.NotSupported(
			"agent_mode="+cfg.AgentMode,
			"must be one of: cloudinit, noagent, auto",
		)
	}
}

// isoStorageSpecDefault is the jobs/pve_cpi/spec default value for
// pve.iso_storage ("local"). BOSH renders spec defaults into the CPI config
// JSON before this process starts (jobs/pve_cpi/templates/cpi.json.erb), so
// the CPI cannot distinguish "operator left iso_storage unset" from
// "operator explicitly typed local" — both arrive as ISOStorage=="local".
// ResolveISOStorage treats that value as the "unset" sentinel when
// iso_storage_follow_vm_storage is enabled; see the field doc on
// config.CPIConfig.ISOStorageFollowVMStorage.
const isoStorageSpecDefault = "local"

// ResolveISOStorage implements the opt-in pve.iso_storage_follow_vm_storage
// resolution. Callers invoke this once at process startup, before NewAgent,
// and use the returned value as the effective iso_storage for the lifetime of
// the process (and for any HA migration-safety checks that also read
// cfg.ISOStorage, so both stay consistent).
//
// Resolution order:
//  1. iso_storage_follow_vm_storage is false (default), or cfg is nil, or the
//     effective agent mode is not cloudinit/auto (ISO storage is unused by
//     noagent) -> cfg.ISOStorage unchanged, zero PVE calls.
//  2. cfg.ISOStorage differs from the spec default "local" -> the operator
//     explicitly pinned a storage pool; returned unchanged, untouched by
//     follow-vm-storage.
//  3. cfg.VMStorage is empty -> nothing to follow; fallback to cfg.ISOStorage
//     with a Warn.
//  4. cfg.VMStorage is present in /storage, advertises content type "iso",
//     and is shared -> returns cfg.VMStorage (follows).
//  5. Any other outcome (vm_storage missing from the index, PVE API error,
//     vm_storage lacks iso content, or vm_storage is not shared) -> fallback
//     to cfg.ISOStorage with a Warn naming the reason. Fail-open: a lookup
//     error never blocks CPI startup.
//
// cfg and logger must not be nil; pveClient may be nil (treated as
// "unavailable", falls back with a Warn) since callers may invoke this before
// a client is fully wired in test harnesses.
func ResolveISOStorage(ctx context.Context, cfg *config.CPIConfig, pveClient pve.Client, logger *log.Logger) string {
	if cfg == nil {
		return ""
	}
	fallback := cfg.ISOStorage

	if !cfg.ISOStorageFollowVMStorageEnabled() {
		return fallback
	}
	if cfg.AgentMode != config.AgentModeCloudInit && cfg.AgentMode != config.AgentModeAuto {
		return fallback
	}
	if fallback != isoStorageSpecDefault {
		// Operator explicitly pinned iso_storage to a non-default pool.
		return fallback
	}
	if cfg.VMStorage == "" {
		logger.Warn("iso_storage_follow_vm_storage: pve.vm_storage is empty, falling back to iso_storage",
			log.String("iso_storage", fallback))
		return fallback
	}
	if ctx == nil || pveClient == nil || pveClient.ClusterStorage() == nil {
		logger.Warn("iso_storage_follow_vm_storage: cluster storage service unavailable, falling back to iso_storage",
			log.String("iso_storage", fallback))
		return fallback
	}

	shared, hasISO, found, lookupErr := vmStorageISOEligibility(ctx, pveClient, cfg.VMStorage)
	switch {
	case lookupErr != nil:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage lookup failed, falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback), log.Err(lookupErr))
		return fallback
	case !found:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage not found in PVE /storage index, falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback))
		return fallback
	case !hasISO:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage does not advertise content type \"iso\", falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback))
		return fallback
	case !shared:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage is not shared, falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback))
		return fallback
	}

	logger.Info("iso_storage_follow_vm_storage: following vm_storage for ConfigDrive ISO",
		log.String("vm_storage", cfg.VMStorage))
	return cfg.VMStorage
}

// vmStorageISOEligibility looks up storageName in /storage and reports
// whether it is shared, advertises PVE content type "iso", and was found at
// all. found is false only when the storage is absent from the index; any
// other lookup failure (transport error, malformed response) is returned as a
// non-nil err with shared/hasISO/found all false.
//
// Decodes each entry through pve.ParseStorageEntry — the same decoder
// StorageInfoCache.refresh and create_vm_disk.go's liveStorageInfo use — so
// this lookup cannot diverge from the canonical field mapping (in
// particular: a fresh, per-entry StorageInfo value, so a storage entry
// missing "shared" or "content" never inherits the previous entry's fields).
func vmStorageISOEligibility(ctx context.Context, pveClient pve.Client, storageName string) (shared, hasISO, found bool, err error) {
	resp, listErr := pveClient.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if listErr != nil {
		return false, false, false, fmt.Errorf("list cluster storage: %w", listErr)
	}
	if resp == nil {
		return false, false, false, fmt.Errorf("nil response from cluster storage list")
	}
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			continue
		}
		if info.Name != storageName {
			continue
		}
		return info.IsShared(), contentIncludesISO(info.Content), true, nil
	}
	return false, false, false, nil
}

// contentIncludesISO reports whether a PVE storage "content" CSV field (e.g.
// "images,iso,vztmpl") lists the "iso" content type as an exact token, not a
// substring match (so a hypothetical "isomorphic" content type would not
// false-positive).
func contentIncludesISO(content string) bool {
	for _, part := range strings.Split(content, ",") {
		if strings.TrimSpace(part) == "iso" {
			return true
		}
	}
	return false
}
