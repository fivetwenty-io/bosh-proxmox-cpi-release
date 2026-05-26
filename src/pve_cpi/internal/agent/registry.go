package agent

import (
	"context"
	"encoding/json"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/registry"
)

// RegistryAgent implements Agent by reading and writing agent settings to the
// BOSH registry service over HTTP. It is selected when agent_mode == "registry".
type RegistryAgent struct {
	reg    *registry.Client
	logger *log.Logger
}

// Compile-time interface satisfaction check.
var _ Agent = (*RegistryAgent)(nil)

// NewRegistryAgent constructs a RegistryAgent backed by reg.
// Both reg and logger must be non-nil.
func NewRegistryAgent(reg *registry.Client, logger *log.Logger) *RegistryAgent {
	if reg == nil {
		panic("registry.Client must not be nil")
	}
	if logger == nil {
		panic("log.Logger must not be nil")
	}
	return &RegistryAgent{reg: reg, logger: logger}
}

// Configure serialises cfg into the canonical BOSH agent settings shape
// (shared with the cloud-init paths via buildSettings) and PUTs the
// result to /instances/{vmid}/settings. Called by create_vm after VM
// creation.
//
// buildSettings applies non-nil networks/disks/env/ntp defaults (so JSON
// renders {}/[] rather than null), VM.Name fallback "vm-{vmid}", and VM.ID
// fallback to the vmid string. cfg.MBus must be set explicitly — an empty
// MBus with a derivable blobstore host returns an error rather than silently
// producing a credential-less NATS URL.
func (a *RegistryAgent) Configure(ctx context.Context, node string, vmid int, cfg AgentConfig) error {
	id := strconv.Itoa(vmid)

	a.logger.Info("registry: configure",
		log.String("node", node),
		log.Int("vmid", vmid),
		log.String("agent_id", cfg.AgentID),
	)

	settings, err := buildSettings(cfg, vmid)
	if err != nil {
		return err
	}

	if err := a.reg.Put(ctx, id, settings); err != nil {
		return cpierrors.Wrap(err, "registry: configure: put settings")
	}
	return nil
}

// Remove deletes registry settings for vmid. A 404 response (already deleted)
// is treated as success so the call is idempotent. Called by delete_vm.
func (a *RegistryAgent) Remove(ctx context.Context, node string, vmid int) error {
	id := strconv.Itoa(vmid)

	a.logger.Info("registry: remove", log.String("node", node), log.Int("vmid", vmid))

	if err := a.reg.Delete(ctx, id); err != nil {
		return cpierrors.Wrap(err, "registry: remove: delete settings")
	}
	return nil
}

// UpdateDiskHints patches the persistent disk map in the stored settings.
// It fetches current settings via GET, merges disks into the persistent map,
// then writes the updated settings back via PUT. Called by attach_disk /
// detach_disk to keep agent disk hints current.
//
// Concurrency: no locking — the BOSH director serialises CPI calls per VM.
func (a *RegistryAgent) UpdateDiskHints(ctx context.Context, vmid int, disks []DiskHint) error {
	id := strconv.Itoa(vmid)

	a.logger.Info("registry: update disk hints",
		log.Int("vmid", vmid),
		log.Int("hints", len(disks)),
	)

	// Step 1: fetch current settings.
	raw, err := a.reg.Get(ctx, id)
	if err != nil {
		return cpierrors.Wrap(err, "registry: update disk hints: get current settings")
	}

	// Step 2: unmarshal into a generic map to preserve unknown fields.
	var current map[string]json.RawMessage
	if err := json.Unmarshal(raw, &current); err != nil {
		return cpierrors.Cloud("registry: update disk hints: unmarshal settings: %s", err.Error())
	}

	// Step 3: extract and decode the disks sub-object.
	var currentDisks DisksSpec
	if disksRaw, ok := current["disks"]; ok {
		if err := json.Unmarshal(disksRaw, &currentDisks); err != nil {
			return cpierrors.Cloud(
				"registry: update disk hints: unmarshal disks field: %s", err.Error(),
			)
		}
	}
	if currentDisks.Persistent == nil {
		currentDisks.Persistent = map[string]string{}
	}

	// Step 4: apply each hint.
	for _, hint := range disks {
		if hint.DiskCID == "" {
			return cpierrors.Cloud("registry: update disk hints: DiskCID must not be empty")
		}
		if hint.DevicePath == "" {
			// Empty DevicePath signals removal of a persistent disk mapping.
			delete(currentDisks.Persistent, hint.DiskCID)
		} else {
			currentDisks.Persistent[hint.DiskCID] = hint.DevicePath
		}
	}

	// Step 5: put the patched disks field back into the raw map.
	patchedDisks, err := json.Marshal(currentDisks)
	if err != nil {
		return cpierrors.Cloud("registry: update disk hints: marshal patched disks: %s", err.Error())
	}
	current["disks"] = json.RawMessage(patchedDisks)

	// Step 6: write back.
	if err := a.reg.Put(ctx, id, current); err != nil {
		return cpierrors.Wrap(err, "registry: update disk hints: put updated settings")
	}
	return nil
}
