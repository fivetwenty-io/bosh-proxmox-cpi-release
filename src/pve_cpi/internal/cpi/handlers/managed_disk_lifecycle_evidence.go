package handlers

import (
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"strings"
)

// managedAttachedVolume verifies identity at the exact returned or planned slot.
// Additional requested drive options must also pass managedConfigFieldsMatch.
func managedAttachedVolume(config map[string]any, slot, volume, stableID string) error {
	if slot == "" || volume == "" {
		return fmt.Errorf("attachment readback requires exact slot and volume")
	}
	value, ok := pve.ConfigString(config, slot)
	if !ok || strings.Split(value, ",")[0] != volume {
		return fmt.Errorf("attachment volume readback mismatch")
	}
	if stableID != "" {
		actual, found := pve.StableIDFromDriveOptStr(value)
		if !found || actual != stableID {
			return fmt.Errorf("attachment serial readback mismatch")
		}
	}
	return nil
}
