package handlers

import (
	"context"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"sort"
	"strconv"
	"strings"
)

type managedDiskLifecycleClient struct {
	pve.Client
	lifecycle *managedDiskLifecycle
}

// unguardedClient reports the client this decorator wraps, which is the
// allocation guard's own decorator, so unguardedPVE can keep walking out to the
// client the CPI built.
func (c *managedDiskLifecycleClient) unguardedClient() pve.Client { return c.Client }

func (c *managedDiskLifecycleClient) QEMU() qemu.Service {
	return &managedDiskLifecycleQEMU{Service: c.Client.QEMU(), client: c.Client, lifecycle: c.lifecycle}
}

type managedDiskLifecycleQEMU struct {
	qemu.Service
	client    pve.Client
	lifecycle *managedDiskLifecycle
}

// DetachDisk exposes both PVE mutations to the journal guard. The SDK helper
// performs its second unused-slot removal internally without a digest boundary.
func (q *managedDiskLifecycleQEMU) DetachDisk(ctx context.Context, node string, vmid int, slot string) error {
	return managedDetachDisk(ctx, q.client, node, vmid, slot, q.lifecycle.disk.volid)
}

// managedDetachDisk requires a guard-decorated client and an independently
// verified volume identity. Each PUT remains a distinct journal boundary.
func managedDetachDisk(ctx context.Context, client pve.Client, node string, vmid int, slot, expectedVolume string) error {
	if expectedVolume == "" {
		return fmt.Errorf("managed detach requires an exact volume identity")
	}

	cfg, err := client.QEMU().Config(ctx, node, vmid)
	if err != nil || cfg == nil {
		return fmt.Errorf("managed detach holder unavailable")
	}
	value, present := pve.ConfigString(cfg, slot)
	if present {
		if strings.Split(value, ",")[0] != expectedVolume {
			return fmt.Errorf("managed detach slot ownership changed")
		}
		if err := managedDeleteSlot(ctx, client, node, vmid, slot, cfg); err != nil {
			return err
		}
	}
	cfg, err = client.QEMU().Config(ctx, node, vmid)
	if err != nil || cfg == nil {
		return fmt.Errorf("managed detach readback unavailable")
	}
	unused := pve.FindUnusedDiskEntries(cfg)
	keys := make([]string, 0, len(unused))
	for key, volume := range unused {
		if volume == expectedVolume {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		// Re-read before each deletion: a previous PUT changes the config digest.
		current, err := client.QEMU().Config(ctx, node, vmid)
		if err != nil || current == nil {
			return fmt.Errorf("managed unused-slot readback unavailable")
		}
		value, exists := pve.ConfigString(current, key)
		if !exists {
			continue
		}
		if strings.Split(value, ",")[0] != expectedVolume {
			return fmt.Errorf("managed unused-slot ownership changed")
		}
		if err := managedDeleteSlot(ctx, client, node, vmid, key, current); err != nil {
			return err
		}
	}
	return nil
}
func managedDeleteSlot(ctx context.Context, client pve.Client, node string, vmid int, slot string, cfg map[string]any) error {
	digest, ok := pve.ConfigString(cfg, "digest")
	if !ok || digest == "" {
		return fmt.Errorf("managed detach requires config generation digest")
	}
	return client.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), &nodes.UpdateQemuConfigParams{Delete: &slot, Digest: &digest})
}
