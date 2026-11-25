package handlers

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// StorageVMExecution records resolved, nonsecret hardware settings. Credential
// values and raw cloud properties are never stored in the plan.
type StorageVMExecution struct {
	Version           int
	InputFingerprint  string
	Node              string
	Storage           string
	StorageType       string
	DiskFormat        string
	RootGiB           int
	MaxAttempts       int
	Cores             int
	Sockets           int
	MemoryMiB         int
	Hotplug           string
	NUMA              bool
	Tags              string
	InitialName       string
	RootOptions       map[string]string
	RootDevice        string
	SCSIController    string
	EphemeralGiB      int
	EphemeralStorage  string
	CPUType           string
	BalloonMiB        *int
	Pool              string
	PoolComment       string
	PoolLayer         string
	PoolDirector      string
	PoolDeployment    string
	PoolInstanceGroup string
	// HAOff records that the create's ha cloud property kept the VM out of
	// PVE HA, so a resumed create clones it as a VM without HA. The key is
	// written only when it is true, so the plan of a VM that keeps HA reads
	// the same as one written before the key existed, and a release without
	// the key, whose strict decode refuses an unknown field, still reads it.
	HAOff bool `json:",omitempty"`
}

func freezeManagedVMExecution(cfg *config.CPIConfig, shape *createVMShape) (*StorageVMExecution, error) {
	if cfg == nil || shape == nil {
		return nil, fmt.Errorf("VM execution requires configuration and shape")
	}
	fallback := cfg.PlacementFallbackMaxValue()
	if fallback < 0 || fallback == math.MaxInt {
		return nil, fmt.Errorf("invalid managed VM retry budget")
	}
	fp, err := managedVMExecutionFingerprint(cfg)
	if err != nil {
		return nil, err
	}
	execution := &StorageVMExecution{Version: 1, InputFingerprint: fp, MaxAttempts: 1 + fallback,
		Node:              shape.node,
		Storage:           shape.vmStorage,
		StorageType:       shape.vmStorageType,
		DiskFormat:        shape.vmDiskFormat,
		RootGiB:           shape.rootDiskGiB,
		Cores:             shape.cores,
		Sockets:           shape.sockets,
		MemoryMiB:         shape.memMiB,
		Hotplug:           shape.hotplug,
		NUMA:              shape.numaEnabled,
		Tags:              shape.initialTags,
		InitialName:       shape.initialName,
		RootOptions:       shape.rootDiskPerfOpts,
		RootDevice:        shape.rootDiskKey,
		SCSIController:    shape.scsihw,
		EphemeralGiB:      shape.ephemeralDiskGiB,
		EphemeralStorage:  shape.ephemeralStorage,
		CPUType:           shape.cpuType,
		BalloonMiB:        shape.balloonMiB,
		Pool:              shape.vmPool,
		PoolComment:       shape.vmPoolComment,
		PoolLayer:         shape.vmPoolLayer,
		PoolDirector:      shape.vmPoolDirector,
		PoolDeployment:    shape.vmPoolDeployment,
		PoolInstanceGroup: shape.vmPoolInstanceGrp,
		HAOff:             shape.haOff,
	}
	execution.RootOptions = maps.Clone(shape.rootDiskPerfOpts)
	if shape.balloonMiB != nil {
		value := *shape.balloonMiB
		execution.BalloonMiB = &value
	}
	return execution, nil
}
func (e *StorageVMExecution) shape(cfg *config.CPIConfig) (*createVMShape, error) {
	if e == nil || e.Version != 1 {
		return nil, fmt.Errorf("recorded VM execution is unsupported")
	}
	matches, err := managedVMExecutionFingerprintMatches(cfg, e.InputFingerprint)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, fmt.Errorf("VM execution defaults changed; reconciliation required")
	}
	shape := &createVMShape{
		node:              e.Node,
		vmStorage:         e.Storage,
		vmStorageType:     e.StorageType,
		vmDiskFormat:      e.DiskFormat,
		rootDiskGiB:       e.RootGiB,
		cores:             e.Cores,
		sockets:           e.Sockets,
		memMiB:            e.MemoryMiB,
		hotplug:           e.Hotplug,
		numaEnabled:       e.NUMA,
		initialTags:       e.Tags,
		initialName:       e.InitialName,
		rootDiskPerfOpts:  e.RootOptions,
		rootDiskKey:       e.RootDevice,
		scsihw:            e.SCSIController,
		ephemeralDiskGiB:  e.EphemeralGiB,
		ephemeralStorage:  e.EphemeralStorage,
		cpuType:           e.CPUType,
		balloonMiB:        e.BalloonMiB,
		vmPool:            e.Pool,
		vmPoolComment:     e.PoolComment,
		vmPoolLayer:       e.PoolLayer,
		vmPoolDirector:    e.PoolDirector,
		vmPoolDeployment:  e.PoolDeployment,
		vmPoolInstanceGrp: e.PoolInstanceGroup,
		haOff:             e.HAOff,
	}
	shape.rootDiskPerfOpts = maps.Clone(e.RootOptions)
	if e.BalloonMiB != nil {
		value := *e.BalloonMiB
		shape.balloonMiB = &value
	}
	return shape, nil
}

// Policy/ranking inputs and current hard storage bounds are revalidated
// separately. Only material non-storage execution settings participate here.
func managedVMExecutionFingerprint(cfg *config.CPIConfig) (string, error) {
	return managedVMExecutionFingerprintOf(cfg, true)
}

// managedVMExecutionFingerprintMatches reports whether stored is the execution
// fingerprint of cfg in any form a release has written. The API transport
// settings (endpoints, endpoint_cache_dir, and api_endpoint_discovery) decide
// how the CPI reaches the PVE API, and release_version names the release that
// rendered the config. Neither decides what the CPI builds, so the fingerprint
// a record stores leaves them out, and a record keeps matching across an
// upgrade, a rollback, and any change to those settings. Two older forms still
// match. A 0.9.x release computes the form with only the transport settings
// left out, because it has none of those keys but does hash release_version,
// so a record it opened matches while the release version is unchanged.
// Releases 0.10.0 through 0.10.2 stored the full form, so a record those
// releases opened matches while every setting, release_version included, is
// unchanged. The ha block stays in every form, because an HA change between
// an attempt and its resume is a change this guard still refuses.
func managedVMExecutionFingerprintMatches(cfg *config.CPIConfig, stored string) (bool, error) {
	for _, form := range []struct{ clearTransport, clearRelease bool }{
		{true, true},
		{false, false},
		{true, false},
	} {
		fp, err := managedVMExecutionFingerprintForm(cfg, form.clearTransport, form.clearRelease)
		if err != nil {
			return false, err
		}
		if stored == fp {
			return true, nil
		}
	}
	return false, nil
}

// managedVMTransportInputs are the top-level CPIConfig keys that choose how
// the CPI reaches the PVE API. A cpi-config entry's overrides land in these
// same fields, so clearing them at the top level covers every entry.
var managedVMTransportInputs = []string{"endpoints", "endpoint_cache_dir", "api_endpoint_discovery"}

// managedVMInformationalInputs are the top-level keys that describe the
// release, not the VM, so they stay out of the stored fingerprint.
var managedVMInformationalInputs = []string{"release_version"}

// managedVMExecutionFingerprintOf computes the stored form when clearStored is
// true and the full form when it is false.
func managedVMExecutionFingerprintOf(cfg *config.CPIConfig, clearStored bool) (string, error) {
	return managedVMExecutionFingerprintForm(cfg, clearStored, clearStored)
}

func managedVMExecutionFingerprintForm(cfg *config.CPIConfig, clearTransport, clearRelease bool) (string, error) {
	// This temporary encoding never leaves memory. RedactSecrets below removes
	// credentials before the only persisted value, the fingerprint, is computed.
	raw, err := json.Marshal(cfg) // #nosec G117 -- input normalization only; redacted before fingerprinting.
	if err != nil {
		return "", err
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", err
	}
	managedVMRemovePlacementInputs(values)
	managedVMNormalizeDiskFormatInput(values)
	if clearTransport {
		for _, key := range managedVMTransportInputs {
			delete(values, key)
		}
	}
	if clearRelease {
		for _, key := range managedVMInformationalInputs {
			delete(values, key)
		}
	}
	return aj.Fingerprint(log.RedactSecrets(values))
}

// managedVMNormalizeDiskFormatInput keeps the fingerprint of an unset
// vm_disk_format equal to the qcow2 that ApplyDefaults used to store for it,
// so a plan frozen before the format came from the storage type still resumes
// after an upgrade. The plan carries its own resolved DiskFormat, so the
// fingerprint does not need to tell an unset value from an explicit qcow2.
func managedVMNormalizeDiskFormatInput(values map[string]any) {
	if s, _ := values["vm_disk_format"].(string); config.IsAutoDiskFormat(s) {
		values["vm_disk_format"] = diskFormatQCOW2
	}
}
func managedVMRemovePlacementInputs(values map[string]any) {
	for key, value := range values {
		switch key {
		case "storage_sets", "storage_capacity_domains", "root_storage_set", "ephemeral_storage_set", "persistent_storage_set", "storage_status_max_age_seconds", "require_disjoint_storage_sets", "storage_placement_namespace", "storage_allocation_journal_dir", "storage_tiers", "vm_storage", "disk_storage", "stemcell_storage", "iso_storage", "iso_storage_follow_vm_storage", "require_shared_iso_for_ha", "placement", "max_utilization_pct", "max_utilization_mode", "reserve_storage_headroom", "storage_headroom_mb", "reserve_mb", "encrypted", "storage_set", "storage_pool", "storage_tier", "ephemeral_storage_pool", "ephemeral_storage_tier":
			delete(values, key)
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			managedVMRemovePlacementInputs(nested)
		}
	}
}
