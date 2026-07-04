package agent

import (
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

	case "noagent":
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
