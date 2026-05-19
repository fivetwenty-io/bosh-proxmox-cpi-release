package agent

import (
	"context"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// NoAgent is the no-op agent strategy. Use when BOSH agent bootstrap is not required
// (e.g. pre-baked stemcells that do not need cloud-init or registry injection).
// All methods log at Debug level and return nil without making any API calls.
type NoAgent struct {
	logger *log.Logger
}

// NewNoAgent constructs a NoAgent. logger must not be nil; pass log.NewNopLogger() for silence.
func NewNoAgent(logger *log.Logger) *NoAgent {
	return &NoAgent{logger: logger}
}

// Configure is a no-op. NoAgent does not write agent settings to the VM.
func (a *NoAgent) Configure(ctx context.Context, node string, vmid int, cfg AgentConfig) error {
	a.logger.Debug("noagent: Configure skipped", log.String("node", node), log.Int("vmid", vmid))
	return nil
}

// Remove is a no-op. NoAgent has no persistent state to clean up.
func (a *NoAgent) Remove(ctx context.Context, node string, vmid int) error {
	a.logger.Debug("noagent: Remove skipped", log.String("node", node), log.Int("vmid", vmid))
	return nil
}

// UpdateDiskHints is a no-op. NoAgent does not maintain a disk hint registry.
func (a *NoAgent) UpdateDiskHints(ctx context.Context, vmid int, disks []DiskHint) error {
	a.logger.Debug("noagent: UpdateDiskHints skipped", log.Int("vmid", vmid), log.Int("disks", len(disks)))
	return nil
}

// compile-time interface guard
var _ Agent = (*NoAgent)(nil)
