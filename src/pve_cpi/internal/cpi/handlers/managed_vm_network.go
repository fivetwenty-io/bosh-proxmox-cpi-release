package handlers

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Stable locally administered unicast MACs keep replayed NIC updates and the
// config-drive payload identical within an allocation generation.
func managedVMNICValue(allocation string, index int, value string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("bosh-pve-vm-nic-v1:%s:%d", allocation, index)))
	mac := fmt.Sprintf("02:%02X:%02X:%02X:%02X:%02X", digest[0], digest[1], digest[2], digest[3], digest[4])
	model, options, found := strings.Cut(value, ",")
	model += "=" + mac
	if found {
		return model + "," + options
	}
	return model
}
