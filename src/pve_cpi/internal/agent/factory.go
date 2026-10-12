package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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
// cfg.AgentMode == "noagent". nodeEndpoints routes ConfigDrive ISO uploads
// directly to the target node's pveproxy; nil disables routing (uploads take
// the proxied path through the configured endpoint). Callers construct it
// alongside the pve.Client so a per-request override bundle's agent routes
// within its own cluster (see handlers.RequestOverrideRuntime.BaseHost).
func NewAgent(cfg *config.CPIConfig, pveClient pve.Client, nodeEndpoints *pve.NodeEndpointResolver, logger *log.Logger) (Agent, error) {
	return NewAgentWithISOResolver(cfg, pveClient, nodeEndpoints, nil, logger)
}

// NewAgentWithISOResolver is NewAgent for a caller that defers the
// iso_storage_follow_vm_storage lookup. When iso is non-nil the ConfigDrive
// asks it for the effective pool on first use, so building the agent issues no
// PVE call. cfg.ISOStorage is still the pool the agent falls back to and the
// one that must be non-empty at construction. A nil iso behaves exactly like
// NewAgent.
func NewAgentWithISOResolver(
	cfg *config.CPIConfig, pveClient pve.Client, nodeEndpoints *pve.NodeEndpointResolver,
	iso *ISOStorageResolver, logger *log.Logger,
) (Agent, error) {
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
		drive := NewConfigDrive(pveClient, isoStorage, nodeEndpoints, logger)
		drive.isoResolver = iso
		return drive, nil

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

// ISOStorageResolver resolves the effective iso_storage pool on first use and
// remembers a conclusive answer. It exists so that building a boot agent, or
// reading the job-level config at process start, issues no PVE call: the
// iso_storage_follow_vm_storage lookup runs against the cluster a request
// actually targets, when a handler first needs the pool.
//
// Only a conclusive answer is remembered: a /storage index that was read, or a
// configuration that needs no lookup. A lookup that failed, or that ran under a
// context that had already ended, returns the configured fallback for that one
// call and is retried by the next, so a transient fault or a short request
// deadline cannot pin a shared-storage cluster to a node-local ISO pool.
//
// It is held by pointer (in the job-level Deps and in each override bundle)
// because Deps is copied by value on every request, and a mutex in a copied
// struct would trip go vet's copylocks check. Concurrent callers share one
// in-flight lookup, and no lock is held across the API call.
type ISOStorageResolver struct {
	mu       sync.Mutex
	cfg      config.CPIConfig
	client   pve.Client
	logger   *log.Logger
	armed    bool
	done     bool
	value    string
	inflight chan struct{}
}

// NewISOStorageResolver snapshots cfg and returns a resolver that calls
// ResolveISOStorage with it on first Resolve. The snapshot keeps later changes
// to the caller's config from changing the answer.
func NewISOStorageResolver(cfg *config.CPIConfig, pveClient pve.Client, logger *log.Logger) *ISOStorageResolver {
	r := &ISOStorageResolver{client: pveClient, logger: logger}
	if cfg != nil {
		r.cfg = *cfg
		r.armed = true
	}
	if r.logger == nil {
		r.logger = log.NewNopLogger()
	}
	return r
}

// Resolve returns the effective iso_storage pool, performing the lookup when no
// conclusive answer is remembered yet. A nil receiver returns "" so callers fall
// back to their configured value. ctx bounds the one /storage call the lookup
// may make, and a caller waiting on another caller's lookup stops waiting when
// its own ctx ends, returning the configured fallback.
func (r *ISOStorageResolver) Resolve(ctx context.Context) string {
	if r == nil || !r.armed {
		return ""
	}
	for {
		r.mu.Lock()
		if r.done {
			value := r.value
			r.mu.Unlock()
			return value
		}
		if wait := r.inflight; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return r.cfg.ISOStorage
			}
		}
		call := make(chan struct{})
		r.inflight = call
		r.mu.Unlock()

		value, conclusive := resolveISOStorage(ctx, &r.cfg, r.client, r.logger)

		r.mu.Lock()
		if conclusive {
			r.done, r.value = true, value
		}
		r.inflight = nil
		r.mu.Unlock()
		close(call)
		return value
	}
}

// ResolveISOStorage implements the opt-in pve.iso_storage_follow_vm_storage
// resolution. Handlers reach it through an ISOStorageResolver, which runs it
// against the cluster a request targets on first use and remembers a
// conclusive answer, so the boot agent and the HA migration-safety checks see
// the same effective iso_storage.
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
	value, _ := resolveISOStorage(ctx, cfg, pveClient, logger)
	return value
}

// resolveISOStorage is ResolveISOStorage that also reports whether the answer
// is conclusive. An answer is inconclusive when it is the fallback because the
// lookup itself failed or because ctx had ended, since a later attempt could
// answer differently. Every other outcome is conclusive: no lookup was needed,
// or the /storage index was read and vm_storage was found eligible, absent, not
// iso-capable, or not shared.
func resolveISOStorage(ctx context.Context, cfg *config.CPIConfig, pveClient pve.Client, logger *log.Logger) (string, bool) {
	if cfg == nil {
		return "", true
	}
	fallback := cfg.ISOStorage

	if !cfg.ISOStorageFollowVMStorageEnabled() {
		return fallback, true
	}
	if cfg.AgentMode != config.AgentModeCloudInit && cfg.AgentMode != config.AgentModeAuto {
		return fallback, true
	}
	if fallback != isoStorageSpecDefault {
		// Operator explicitly pinned iso_storage to a non-default pool.
		return fallback, true
	}
	if cfg.VMStorage == "" {
		logger.Warn("iso_storage_follow_vm_storage: pve.vm_storage is empty, falling back to iso_storage",
			log.String("iso_storage", fallback))
		return fallback, true
	}
	if ctx == nil || pveClient == nil || pveClient.ClusterStorage() == nil {
		logger.Warn("iso_storage_follow_vm_storage: cluster storage service unavailable, falling back to iso_storage",
			log.String("iso_storage", fallback))
		return fallback, true
	}

	shared, hasISO, found, lookupErr := vmStorageISOEligibility(ctx, pveClient, cfg.VMStorage)
	switch {
	case lookupErr != nil:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage lookup failed, falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback), log.Err(lookupErr))
		return fallback, false
	case ctx.Err() != nil:
		return fallback, false
	case !found:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage not found in PVE /storage index, falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback))
		return fallback, true
	case !hasISO:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage does not advertise content type \"iso\", falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback))
		return fallback, true
	case !shared:
		logger.Warn("iso_storage_follow_vm_storage: vm_storage is not shared, falling back to iso_storage",
			log.String("vm_storage", cfg.VMStorage), log.String("iso_storage", fallback))
		return fallback, true
	}

	logger.Info("iso_storage_follow_vm_storage: following vm_storage for ConfigDrive ISO",
		log.String("vm_storage", cfg.VMStorage))
	return cfg.VMStorage, true
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
