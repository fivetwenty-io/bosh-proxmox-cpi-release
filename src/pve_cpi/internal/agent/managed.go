package agent

import (
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ManagedAgentEvidence proves ISO identity and payload before reuse.
type ManagedAgentEvidence struct {
	ExistingISO  string
	CheckPayload func([]byte) error
}

// NewManagedAgent binds the original-config-preserving resolved ISO policy to
// the request's guarded client. It rejects an existing ISO rather than deleting
// unknown data and verifies artifact size against the frozen plan before upload.
func NewManagedAgent(cfg *config.CPIConfig, client pve.Client, endpoints *pve.NodeEndpointResolver, logger *log.Logger, allocationBytes uint64, evidence ...ManagedAgentEvidence) (Agent, error) {
	if cfg != nil && cfg.AgentMode == config.AgentModeAuto {
		resolved := *cfg
		resolved.AgentMode = config.AgentModeCloudInit
		cfg = &resolved
	}
	chosen, err := NewAgent(cfg, client, endpoints, logger)
	if err != nil {
		return nil, err
	}
	if drive, ok := chosen.(*ConfigDrive); ok {
		if allocationBytes != configdrive.AllocationBytes() {
			return nil, cpierrors.Cloud("managed configdrive requires the builder's frozen allocation size")
		}
		drive.managedAllocationBytes = allocationBytes
		if len(evidence) > 1 {
			return nil, cpierrors.Cloud("managed agent requires one evidence context")
		}
		if len(evidence) == 1 {
			drive.managedExistingISO = evidence[0].ExistingISO
			drive.managedPayloadCheck = evidence[0].CheckPayload
			if drive.managedExistingISO != "" && drive.managedPayloadCheck == nil {
				return nil, cpierrors.Cloud("managed ISO reuse requires payload proof")
			}
		}
	}
	return chosen, nil
}
