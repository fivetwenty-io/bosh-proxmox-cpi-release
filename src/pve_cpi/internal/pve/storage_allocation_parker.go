package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// VerifyAllocationParked proves the complete ownership identity after parking.
// Advisory legacy provenance writes are insufficient for a managed disk CID.
func VerifyAllocationParked(ctx context.Context, c Client, logger *log.Logger, volume, token, namespace, id string, cfg ParkerConfig) error {
	identity, err := ResolveDiskIdentity(ctx, c, logger, volume, token, cfg)
	if err != nil {
		return err
	}
	if !identity.Holder.Found || !identity.Holder.IsParker || identity.Volid != volume || identity.Intent != nil {
		return fmt.Errorf("allocation disk is not held by its verified parker")
	}
	vm, err := c.QEMU().Config(ctx, identity.Holder.Node, identity.Holder.VMID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("nil parker readback")
	}
	drive, ok := ConfigString(vm, identity.Holder.Slot)
	serial, hasSerial := StableIDFromDriveOptStr(drive)
	if !ok || strings.Split(drive, ",")[0] != volume || !hasSerial || serial != token {
		return fmt.Errorf("parker drive identity mismatch")
	}
	_, raw := ParseSentinel(DescriptionFromConfig(vm))
	encoded, ok := raw["bosh_parked_disks"]
	if !ok {
		return fmt.Errorf("parker allocation provenance absent")
	}
	var entries map[string]parkerProvEntry
	if err = json.Unmarshal(encoded, &entries); err != nil {
		return fmt.Errorf("malformed parker allocation provenance")
	}
	entry, ok := entries[token]
	if !ok || entry.AllocationID != id || entry.AllocationNamespace != namespace || entry.Volid != volume || entry.Slot != identity.Holder.Slot {
		return fmt.Errorf("parker allocation provenance mismatch")
	}
	return nil
}
