package agent

import (
	"net/url"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/registry"
)

// NewAgent returns the Agent implementation selected by cfg.AgentMode.
//
//   - "cloudinit" → ConfigDrive using pveClient; storage is cfg.ISOStorage when
//     non-empty, else cfg.StemcellStorage, else cfg.VMStorage.
//   - "registry"  → RegistryAgent; builds a registry.Client from cfg.RegistryEndpoint,
//     cfg.RegistryUser, and cfg.RegistryPassword. Returns an error if RegistryEndpoint
//     is empty (defensive guard — config validation should catch this first).
//   - "noagent"   → NoAgent (no-op).
//   - any other   → *errors.Error of type NotSupported.
//
// cfg and logger must not be nil. pveClient may be nil only when cfg.AgentMode == "registry"
// or cfg.AgentMode == "noagent".
func NewAgent(cfg *config.CPIConfig, pveClient pve.Client, logger *log.Logger) (Agent, error) {
	if cfg == nil {
		return nil, cpierrors.Cloud("agent.NewAgent: cfg must not be nil")
	}
	if logger == nil {
		return nil, cpierrors.Cloud("agent.NewAgent: logger must not be nil")
	}

	switch cfg.AgentMode {
	case "cloudinit":
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

	case "registry":
		if cfg.RegistryEndpoint == "" {
			return nil, cpierrors.Cloud("agent.NewAgent: registry_endpoint is required when agent_mode=registry")
		}
		regClient, err := registry.NewClientWithOptions(
			cfg.RegistryEndpoint, cfg.RegistryUser, cfg.RegistryPassword,
			registry.Options{
				CACertPEM:      cfg.RegistryCACertPEM,
				AllowedHosts:   cfg.RegistryAllowedHosts,
				AllowPrivateIP: cfg.RegistryAllowPrivateIPValue(),
			},
		)
		if err != nil {
			return nil, cpierrors.Cloud("agent.NewAgent: build registry client: %s", err.Error())
		}
		return NewRegistryAgent(regClient, logger), nil

	case "noagent":
		// Warn on misconfiguration: noagent ignores registry + ISO storage
		// settings. Catching this here makes the operator-error visible
		// before VM creation rather than after silent ignore.
		if cfg.RegistryEndpoint != "" {
			logger.Warn("agent.NewAgent: agent_mode=noagent ignores registry_endpoint",
				log.String("registry_endpoint", redactURL(cfg.RegistryEndpoint)))
		}
		if cfg.ISOStorage != "" {
			logger.Warn("agent.NewAgent: agent_mode=noagent ignores iso_storage",
				log.String("iso_storage", cfg.ISOStorage))
		}
		return NewNoAgent(logger), nil

	default:
		return nil, cpierrors.NotSupported(
			"agent_mode="+cfg.AgentMode,
			"must be one of: cloudinit, registry, noagent",
		)
	}
}

// NewRegistryAgentIfConfigured builds a registry-backed Agent when
// cfg.RegistryEndpoint is non-empty, or returns (nil, nil) when it is empty.
// This is the opt-in secondary registry path for agent_mode=auto: callers
// must fall back to their primary agent when this returns nil.
//
// Registry client construction mirrors the "registry" case in NewAgent exactly
// (CACertPEM, AllowedHosts, AllowPrivateIP). Returns (nil, CloudError) when
// the endpoint is set but NewClientWithOptions rejects it.
//
// cfg and logger must not be nil.
func NewRegistryAgentIfConfigured(cfg *config.CPIConfig, logger *log.Logger) (Agent, error) {
	if cfg == nil {
		return nil, cpierrors.Cloud("agent.NewRegistryAgentIfConfigured: cfg must not be nil")
	}
	if logger == nil {
		return nil, cpierrors.Cloud("agent.NewRegistryAgentIfConfigured: logger must not be nil")
	}
	if cfg.RegistryEndpoint == "" {
		return nil, nil
	}
	regClient, err := registry.NewClientWithOptions(
		cfg.RegistryEndpoint, cfg.RegistryUser, cfg.RegistryPassword,
		registry.Options{
			CACertPEM:      cfg.RegistryCACertPEM,
			AllowedHosts:   cfg.RegistryAllowedHosts,
			AllowPrivateIP: cfg.RegistryAllowPrivateIPValue(),
		},
	)
	if err != nil {
		return nil, cpierrors.Cloud("agent.NewRegistryAgentIfConfigured: build registry client: %s", err.Error())
	}
	return NewRegistryAgent(regClient, logger), nil
}

// redactURL strips userinfo from a URL before logging. A parse failure returns
// the original string so the operator still sees something useful in logs.
func redactURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	if u.User != nil {
		u.User = url.UserPassword("REDACTED", "REDACTED")
	}
	return u.String()
}
