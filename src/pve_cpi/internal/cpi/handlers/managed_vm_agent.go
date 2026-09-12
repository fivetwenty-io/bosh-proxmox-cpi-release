package handlers

import (
	"encoding/json"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// checkAgentPayload freezes only a credential-excluded content digest. Reusing
// an observed ISO requires the same logical settings and never uploads again.
func (m *managedVMAllocation) checkAgentPayload(payload []byte) error {
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return fmt.Errorf("invalid managed agent payload")
	}
	fingerprint, err := aj.Fingerprint(log.RedactSecrets(value))
	if err != nil {
		return err
	}
	kind := "vm.agent_payload." + fingerprint
	recorded := m.handle.Record()
	for i := range recorded.Steps {
		step := &recorded.Steps[i]
		if step.Attempt != recorded.ActiveAttempt() || !strings.HasPrefix(step.Kind, "vm.agent_payload.") {
			continue
		}
		if step.Kind != kind || step.State != aj.Observed {
			return fmt.Errorf("managed agent settings differ from frozen generation")
		}
		return nil
	}
	if m.volumes[storageRoleISO] != "" {
		return fmt.Errorf("recorded ISO lacks immutable agent settings proof")
	}
	step, err := storageMutationIntent(m.handle, kind, aj.Target{Node: m.shape.node, VMID: m.vmid}, nil)
	if err != nil {
		return err
	}
	return storageMutationObserved(m.handle, step, nil, false)
}
