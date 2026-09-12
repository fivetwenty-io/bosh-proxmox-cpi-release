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
	fp, err := managedVMExecutionFingerprint(cfg)
	if err != nil {
		return nil, err
	}
	if fp != e.InputFingerprint {
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
	return aj.Fingerprint(log.RedactSecrets(values))
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
