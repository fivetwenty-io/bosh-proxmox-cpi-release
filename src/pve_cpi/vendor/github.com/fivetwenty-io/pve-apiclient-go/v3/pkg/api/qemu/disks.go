package qemu

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	errUnsupportedBus = errors.New("unsupported bus")
	errSizeNonZero    = errors.New("sizeGiB must be non-zero")
)

// AttachOpts controls disk attach behavior.
type AttachOpts struct {
	// DiskID optionally sets the exact disk key (e.g., "scsi1").
	DiskID string
	// Extra allows additional VM config parameters alongside the disk assignment.
	Extra map[string]interface{}
}

// AttachDisk attaches a volume to a VM, computing the next index for the bus if needed.
func (s *service) AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *AttachOpts) (string, error) {
	err := s.validateBus(bus)
	if err != nil {
		return "", err
	}

	diskID, err := s.determineDiskID(ctx, node, vmid, volid, strings.ToLower(bus), opts)
	if err != nil {
		return "", err
	}

	params := s.buildAttachParams(diskID, volid, opts)

	err = s.attachDiskToVM(ctx, node, vmid, params)
	if err != nil {
		return "", err
	}

	return diskID, nil
}

func (s *service) validateBus(bus string) error {
	bus = strings.ToLower(bus)
	switch bus {
	case "scsi", "virtio", "ide", "sata":
		return nil
	default:
		return fmt.Errorf("%w: %s", errUnsupportedBus, bus)
	}
}

func (s *service) determineDiskID(ctx context.Context, node string, vmid int, volid, bus string, opts *AttachOpts) (string, error) {
	if opts != nil && opts.DiskID != "" {
		return opts.DiskID, nil
	}

	cfg, err := s.Config(ctx, node, vmid)
	if err != nil {
		return "", err
	}

	if existing, ok := FindDiskIDByVolID(cfg, volid); ok {
		return existing, nil
	}

	return fmt.Sprintf("%s%d", bus, NextIndexForBus(cfg, bus)), nil
}

func (s *service) buildAttachParams(diskID, volid string, opts *AttachOpts) map[string]interface{} {
	params := map[string]interface{}{diskID: volid}

	if opts != nil && opts.Extra != nil {
		for k, v := range opts.Extra {
			params[k] = v
		}
	}

	return params
}

func (s *service) attachDiskToVM(ctx context.Context, node string, vmid int, params map[string]interface{}) error {
	_, err := s.c.PutCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), params)
	if err != nil {
		return fmt.Errorf("failed to attach disk to VM %d on node %q: %w", vmid, node, err)
	}

	return nil
}

// DetachDisk detaches a disk by its diskID (e.g., scsi0).
//
// PVE's PUT /qemu/{vmid}/config with `delete: scsiN` removes the disk from
// its bus slot but moves the volume reference into a new `unusedN` slot
// rather than fully clearing it. The volume remains attached to the VM's
// configuration; a subsequent DELETE /qemu/{vmid} (or `qm destroy --purge`)
// will then destroy every disk still referenced — unusedN included — and
// silently nuke the persistent volume.
//
// To make "detach" mean detached, this method performs a second config PUT
// that removes the resulting unusedN slot. Callers therefore never observe
// the dangling unused entry, and any later destroy of the VM will not touch
// the volume. When diskID itself names an `unusedN` slot, only the single
// delete is issued.
//
// No-op when diskID is not present in the VM config.
func (s *service) DetachDisk(ctx context.Context, node string, vmid int, diskID string) error {
	cfg, err := s.Config(ctx, node, vmid)
	if err != nil {
		return err
	}

	rawVal, ok := cfg[diskID]
	if !ok {
		return nil
	}

	volid, _ := rawVal.(string)

	configPath := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid)

	_, err = s.c.PutCtx(ctx, configPath, map[string]interface{}{"delete": diskID})
	if err != nil {
		return fmt.Errorf("failed to detach disk %q from VM %d on node %q: %w", diskID, vmid, node, err)
	}

	// If we were already deleting an unusedN slot, or the original config entry
	// did not carry a parsable volid, the auto-move side-effect does not apply.
	if strings.HasPrefix(diskID, "unused") || volid == "" {
		return nil
	}

	return s.clearAutoUnusedSlot(ctx, node, vmid, diskID, volid, configPath)
}

// clearAutoUnusedSlot removes the unusedN slot PVE auto-creates when a disk
// is detached from its bus slot. This prevents a subsequent VM destroy from
// silently deleting the volume that was intentionally detached.
func (s *service) clearAutoUnusedSlot(ctx context.Context, node string, vmid int, diskID, volid, configPath string) error {
	// Sweep the unusedN slot PVE auto-creates for the bare volid prefix
	// (config values may be "<volid>" or "<volid>,options").
	bareVolid := volid
	if comma := strings.Index(volid, ","); comma >= 0 {
		bareVolid = volid[:comma]
	}

	cfg2, err := s.Config(ctx, node, vmid)
	if err != nil {
		return fmt.Errorf("failed to refresh config after detaching disk %q from VM %d on node %q: %w", diskID, vmid, node, err)
	}

	for key, raw := range cfg2 {
		if !strings.HasPrefix(key, "unused") {
			continue
		}

		val, ok := raw.(string)
		if !ok || val == "" {
			continue
		}

		valBare := val
		if comma := strings.Index(val, ","); comma >= 0 {
			valBare = val[:comma]
		}

		if valBare != bareVolid {
			continue
		}

		_, err = s.c.PutCtx(ctx, configPath, map[string]interface{}{"delete": key})
		if err != nil {
			return fmt.Errorf("failed to remove unused slot %q for volid %q on VM %d node %q: %w", key, bareVolid, vmid, node, err)
		}

		break
	}

	return nil
}

// ResizeDisk resizes a disk and returns the UPID.
func (s *service) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error) {
	if sizeGiB == 0 {
		// No-op resize still calls API, but guard against accidental zero.
		return "", errSizeNonZero
	}

	params := map[string]interface{}{
		"disk": diskID,
		"size": fmt.Sprintf("+%dG", sizeGiB),
	}

	// PVE's /resize endpoint uses PUT.
	data, err := s.c.PutCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/resize", url.PathEscape(node), vmid), params)
	if err != nil {
		return "", fmt.Errorf("failed to execute QEMU operation: %w", err)
	}

	if upid, ok := data.(string); ok {
		return upid, nil
	}

	if m, ok := data.(map[string]interface{}); ok {
		if v, ok := m["upid"].(string); ok {
			return v, nil
		}
	}

	return "", nil
}
