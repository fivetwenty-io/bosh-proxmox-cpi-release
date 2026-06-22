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
func (a *NoAgent) Configure(_ context.Context, node string, vmid int, _ AgentConfig) error {
	a.logger.Debug("noagent: Configure skipped", log.String("node", node), log.Int("vmid", vmid))
	return nil
}

// Remove is a no-op. NoAgent has no persistent state to clean up.
func (a *NoAgent) Remove(_ context.Context, node string, vmid int) error {
	a.logger.Debug("noagent: Remove skipped", log.String("node", node), log.Int("vmid", vmid))
	return nil
}

// compile-time interface guard
var _ Agent = (*NoAgent)(nil)
