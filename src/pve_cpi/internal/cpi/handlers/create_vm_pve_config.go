package handlers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// pveConfigKeyCPU is the PVE VM config key for the emulated CPU type.
// Defined as a constant to satisfy goconst (the literal "cpu" appears in
// hotplug token logic and elsewhere in the package).
const pveConfigKeyCPU = "cpu"

// pveConfigKeyMachine is the PVE VM config key for the QEMU machine type.
// Defined as a constant to satisfy goconst (the literal "machine" appears
// multiple times across allowlist, switch, and test code).
const pveConfigKeyMachine = "machine"

// pveConfigAllowlist is the set of PVE VM config keys an operator may supply
// via cloud_properties.pve_config. Keys here are safe to pass through because
// the CPI does not own them and they do not affect disk/network slot layout.
//
// "numa" is intentionally excluded: the memory_hotplug resolver sets numa=1
// when memory hotplug is enabled, so operator passthrough would conflict with
// CPI-managed state. "args" is excluded as an execution surface.
var pveConfigAllowlist = map[string]struct{}{
	pveConfigKeyMachine: {},
	"bios":              {},
	pveConfigKeyCPU:     {},
}

// pveConfigBlocklist is the set of PVE VM config keys the CPI manages
// directly. Specifying any of these in pve_config is a non-retriable operator
// error — the CPI controls their values and a passthrough would fight its own
// decisions. A blocklisted key produces a more specific error message than an
// unlisted key, but both are rejected.
//
// Index-based keys are represented as prefixes (net, scsi, ide, virtio) and
// matched by prefix in validatePVEConfigKey.
var pveConfigBlocklist = map[string]struct{}{
	"cores":         {},
	"memory":        {},
	"sockets":       {},
	"boot":          {},
	metadataKeyName: {},
	jsonKeyTags:          {},
	"hotplug":       {},
	"numa":          {},
	"smbios1":       {},
	"agent":         {},
	"onboot":        {},
	"tablet":        {},
	"vmgenid":       {},
	"description":   {},
	"ostype":        {},
	"args":          {},
}

// pveConfigBlocklistPrefixes matches indexed PVE keys (net0..net9,
// scsi0..scsi30, ide0..ide3, virtio0..virtio15).
var pveConfigBlocklistPrefixes = []string{"net", "scsi", "ide", "virtio"}

// pveConfigMetacharPattern holds the shell metacharacters rejected in
// pve_config values. Values travel via the PVE REST API (not a shell), but
// PVE config values can reach host tooling, so this is a defence-in-depth
// guard against injection.
const pveConfigMetacharPattern = `;&|$` + "`" + `<>`

// validatePVEConfig runs key and value validation over the whole cfg map and
// returns the first non-retriable CloudError found. Called from
// parseCreateVMArgs (pre-clone) so a bad pve_config rejects the call before
// any VM is created, producing no orphan. Nil or empty cfg is a no-op.
func validatePVEConfig(cfg map[string]string) error {
	if len(cfg) == 0 {
		return nil
	}
	// Iterate in sorted order for deterministic error messages.
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := validatePVEConfigKey(k); err != nil {
			return err
		}
		if err := validatePVEConfigValue(k, cfg[k]); err != nil {
			return err
		}
	}
	return nil
}

// applyPVEConfigWithCleanup applies a pre-validated pve_config map to the VM
// and, on any error, immediately calls cleanupVM to destroy the candidate VM
// before returning the error. Use this variant inside attemptCreateVM where
// rollbackOnExit is not yet armed (the defer fires only after allocateVM
// returns successfully to createVM). On success it returns nil without
// touching the VM.
func applyPVEConfigWithCleanup(
	ctx context.Context,
	deps Deps,
	node string,
	candidate int,
	cfg map[string]string,
	logger *log.Logger,
) error {
	if err := applyPVEConfigPassthrough(ctx, deps, node, candidate, cfg, logger); err != nil {
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, nil, logger)
		return err
	}
	return nil
}

// applyPVEConfigPassthrough applies a pre-validated pve_config map to the VM
// via a single UpdateQemuConfig call. Callers MUST call validatePVEConfig
// before any VM is created (parseCreateVMArgs does this); this function
// assumes cfg has already passed validation.
//
// Nil or empty cfg is a no-op (byte-identical to prior behavior; no API call).
//
// On API error the error is wrapped retriable via pve.WrapError. Callers
// inside attemptCreateVM MUST call cleanupVM for the candidate VMID before
// returning any error from this function, because at that point the VM exists
// but rollbackOnExit has not yet been armed (that defer fires only after
// allocateVM returns to createVM).
//
// Keys are applied in sorted order for determinism.
func applyPVEConfigPassthrough(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	cfg map[string]string,
	logger *log.Logger,
) error {
	if len(cfg) == 0 {
		return nil
	}

	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build a single UpdateQemuConfigParams. cfg was validated at parse time;
	// map each allowlisted key to its typed SDK field.
	params := &sdknodes.UpdateQemuConfigParams{}
	for _, k := range keys {
		v := cfg[k]
		val := v // capture loop variable
		switch k {
		case pveConfigKeyMachine:
			params.Machine = &val
		case "bios":
			params.Bios = &val
		case pveConfigKeyCPU:
			params.Cpu = &val
			// pve_config.cpu is the raw escape hatch and always wins as the
			// final write (this call runs after the create/clone step that
			// applies pve.cpu_type / cloud_properties.cpu_type). Point the
			// operator at the first-class knob in case the raw passthrough
			// was only used because cpu_type was not yet known to exist.
			logger.Info(
				"create_vm: pve_config.cpu is set; note pve.cpu_type / cloud_properties.cpu_type "+
					"exist as the first-class knob for the emulated CPU type and take effect before "+
					"this raw override is applied",
				log.Int(metadataKeyVMID, vmid),
			)
		}
	}

	vmidStr := strconv.Itoa(vmid)
	if cfgErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmidStr, params); cfgErr != nil {
		return cpierrors.Wrap(pve.WrapError(cfgErr),
			fmt.Sprintf("create_vm: apply pve_config to vmid=%d: %s", vmid, cfgErr.Error()))
	}

	logger.Info("create_vm: applied pve_config passthrough",
		log.Int(metadataKeyVMID, vmid),
		log.Int("key_count", len(keys)),
	)
	return nil
}

// validatePVEConfigKey returns a non-retriable CloudError when k is
// blocklisted (CPI-managed) or not in the allowlist (unknown/unsupported).
func validatePVEConfigKey(k string) error {
	// Exact blocklist check first — gives a more specific message.
	if _, blocked := pveConfigBlocklist[k]; blocked {
		return cpierrors.Cloud(
			"create_vm: pve_config key %q is managed by the CPI and cannot be set via pve_config; "+
				"remove it from cloud_properties.pve_config",
			k,
		)
	}
	// Prefix-based blocklist for indexed keys (net0, scsi1, virtio0, ide2, …).
	for _, pfx := range pveConfigBlocklistPrefixes {
		if strings.HasPrefix(k, pfx) {
			return cpierrors.Cloud(
				"create_vm: pve_config key %q matches CPI-managed prefix %q and cannot be set via pve_config; "+
					"remove it from cloud_properties.pve_config",
				k, pfx,
			)
		}
	}
	// Allowlist check — anything not explicitly allowed is rejected.
	if _, ok := pveConfigAllowlist[k]; !ok {
		return cpierrors.Cloud(
			"create_vm: pve_config key %q is not in the allowed key set (machine, bios, cpu); "+
				"remove it from cloud_properties.pve_config or request the key be added to the allowlist",
			k,
		)
	}
	return nil
}

// validatePVEConfigValue returns a non-retriable CloudError when v is empty
// or contains shell metacharacters. An empty value would blank the PVE field,
// which is never the operator's intent; values with shell metacharacters are
// rejected as defence-in-depth (PVE config values can reach host tooling).
func validatePVEConfigValue(k, v string) error {
	if v == "" {
		return cpierrors.Cloud(
			"create_vm: pve_config key %q has an empty value; provide a non-empty value or remove the key",
			k,
		)
	}
	if strings.ContainsAny(v, pveConfigMetacharPattern) {
		return cpierrors.Cloud(
			"create_vm: pve_config key %q has a value containing a shell metacharacter; "+
				"remove shell metacharacters from the value",
			k,
		)
	}
	return nil
}
