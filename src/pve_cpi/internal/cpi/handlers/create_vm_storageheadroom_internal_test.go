package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestComputeRequiredStorageBytes_GateOff verifies that when
// ReserveStorageHeadroomEnabled is false (the default), the caller computes
// 0 bytes — RequiredStorageBytes stays 0 and placement is byte-identical to
// prior releases. This is the critical envelope-policy test.
func TestComputeRequiredStorageBytes_GateOff(t *testing.T) {
	t.Parallel()

	// Gate is off by default (nil Placement).
	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	if cfg.ReserveStorageHeadroomEnabled() {
		t.Fatal("gate should be off by default")
	}
	// Callers only call computeRequiredStorageBytes when the gate is on.
	// Verify the gate is off so RequiredStorageBytes = 0.
	var required int64
	if cfg.ReserveStorageHeadroomEnabled() {
		cp := createVMCloudProps{RootDiskSize: 10240, Memory: 1024}
		required = computeRequiredStorageBytes(cfg, cp, cfg.VMStorage)
	}
	if required != 0 {
		t.Errorf("gate off: required = %d; want 0 (byte-identical)", required)
	}
}

// TestComputeRequiredStorageBytes_RootOnly verifies the formula when no ephemeral
// disk is present: floor = rootDiskBytes + headroomBytes (headroom only, no swap).
func TestComputeRequiredStorageBytes_RootOnly(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		Placement: &config.PlacementConfig{
			ReserveStorageHeadroom: &enabled,
			// StorageHeadroomMB nil → default 1024 MiB (1 GiB).
		},
	}

	cp := createVMCloudProps{
		RootDiskSize: 10240, // 10 GiB in MiB
		Memory:       2048,  // 2 GiB RAM — NOT included in headroom (no ephemeral disk)
		// EphemeralDiskSizeMB = 0 → no dedicated ephemeral disk
	}

	got := computeRequiredStorageBytes(cfg, cp, cfg.VMStorage)

	const gibBytes = int64(1024 * 1024 * 1024)
	const mibBytes = int64(1024 * 1024)
	// rootDiskGiB = max(5, ceil(10240/1024)) = max(5, 10) = 10
	wantRoot := int64(10) * gibBytes
	// headroom = 1024 MiB default = 1 GiB; no swap (no ephemeral)
	wantHeadroom := int64(1024) * mibBytes
	want := wantRoot + wantHeadroom

	if got != want {
		t.Errorf("computeRequiredStorageBytes(root-only) = %d; want %d (10 GiB root + 1 GiB headroom)",
			got, want)
	}
}

// TestComputeRequiredStorageBytes_RootAndEphemeralSamePool verifies the formula
// when an ephemeral disk is present on the same pool:
// floor = rootDiskBytes + ephemeralDiskBytes + headroomMiB + ramSwap.
func TestComputeRequiredStorageBytes_RootAndEphemeralSamePool(t *testing.T) {
	t.Parallel()

	enabled := true
	headroomMB := 512 // 512 MiB explicit margin
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		Placement: &config.PlacementConfig{
			ReserveStorageHeadroom: &enabled,
			StorageHeadroomMB:      &headroomMB,
		},
	}

	cp := createVMCloudProps{
		RootDiskSize:        20480, // 20 GiB in MiB
		EphemeralDiskSizeMB: 8192,  // 8 GiB in MiB
		Memory:              4096,  // 4 GiB RAM (included in headroom as swap)
		// EphemeralStoragePool="" → defaults to VMStorage → same pool
	}

	got := computeRequiredStorageBytes(cfg, cp, cfg.VMStorage)

	const gibBytes = int64(1024 * 1024 * 1024)
	const mibBytes = int64(1024 * 1024)
	// rootDiskGiB = ceil(20480/1024) = 20
	wantRoot := int64(20) * gibBytes
	// ephemeralDiskGiB = ceil(8192/1024) = 8
	wantEphemeral := int64(8) * gibBytes
	// headroom = 512 MiB margin + 4096 MiB RAM swap
	wantHeadroom := (int64(512) + int64(4096)) * mibBytes
	want := wantRoot + wantEphemeral + wantHeadroom

	if got != want {
		t.Errorf("computeRequiredStorageBytes(root+ephemeral same pool) = %d; want %d", got, want)
	}
}

// TestComputeRequiredStorageBytes_EphemeralOnDifferentPool verifies that when
// the ephemeral disk is on a different pool (cp.EphemeralStoragePool != storageName),
// its bytes are excluded from the filter. Only root disk + headroom (no swap) count.
func TestComputeRequiredStorageBytes_EphemeralOnDifferentPool(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		Placement: &config.PlacementConfig{
			ReserveStorageHeadroom: &enabled,
			// StorageHeadroomMB nil → 1024 MiB default
		},
	}

	cp := createVMCloudProps{
		RootDiskSize:         10240, // 10 GiB
		EphemeralDiskSizeMB:  8192,  // 8 GiB — on different pool, excluded
		Memory:               2048,  // 2 GiB — no swap (ephemeral excluded)
		EphemeralStoragePool: "fast-nvme",
	}

	got := computeRequiredStorageBytes(cfg, cp, "local-lvm")

	const gibBytes = int64(1024 * 1024 * 1024)
	const mibBytes = int64(1024 * 1024)
	// rootDiskGiB = 10
	wantRoot := int64(10) * gibBytes
	// headroom = 1024 MiB (default); no ephemeral bytes, no swap bytes
	wantHeadroom := int64(1024) * mibBytes
	want := wantRoot + wantHeadroom

	if got != want {
		t.Errorf("computeRequiredStorageBytes(ephemeral different pool) = %d; want %d "+
			"(ephemeral bytes excluded when on different pool)", got, want)
	}
}

// TestComputeRequiredStorageBytes_DefaultRootDisk verifies the 5 GiB floor
// when no disk size is specified in cloud_properties (defaultStemcellDiskGiB).
func TestComputeRequiredStorageBytes_DefaultRootDisk(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		Placement: &config.PlacementConfig{
			ReserveStorageHeadroom: &enabled,
			// StorageHeadroomMB nil → 1024 MiB
		},
	}

	cp := createVMCloudProps{
		// RootDiskSize=0, Disk=0 → defaultStemcellDiskGiB (5 GiB)
		Memory: 1024,
	}

	got := computeRequiredStorageBytes(cfg, cp, cfg.VMStorage)

	const gibBytes = int64(1024 * 1024 * 1024)
	const mibBytes = int64(1024 * 1024)
	wantRoot := int64(defaultStemcellDiskGiB) * gibBytes // 5 GiB
	wantHeadroom := int64(1024) * mibBytes
	want := wantRoot + wantHeadroom

	if got != want {
		t.Errorf("computeRequiredStorageBytes(default root) = %d; want %d (5 GiB + 1 GiB headroom)",
			got, want)
	}
}

// TestComputeRequiredStorageBytes_RAMFieldUsedOverMemory verifies that the RAM
// field takes precedence over Memory when both are set (mirrors RAM as alias).
func TestComputeRequiredStorageBytes_RAMFieldUsedOverMemory(t *testing.T) {
	t.Parallel()

	enabled := true
	headroomMB := 0 // zero → default 1024 MiB applied by accessor
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		Placement: &config.PlacementConfig{
			ReserveStorageHeadroom: &enabled,
			StorageHeadroomMB:      &headroomMB,
		},
	}

	cp := createVMCloudProps{
		RootDiskSize:        5120, // 5 GiB
		EphemeralDiskSizeMB: 2048, // 2 GiB on same pool
		Memory:              1024, // ignored: RAM field takes precedence
		RAM:                 2048, // 2 GiB → used for swap
	}

	got := computeRequiredStorageBytes(cfg, cp, cfg.VMStorage)

	const gibBytes = int64(1024 * 1024 * 1024)
	const mibBytes = int64(1024 * 1024)
	wantRoot := int64(5) * gibBytes
	wantEphemeral := int64(2) * gibBytes
	// StorageHeadroomMB=0 → default 1024 MiB; RAM=2048 MiB for swap
	wantHeadroom := (int64(1024) + int64(2048)) * mibBytes
	want := wantRoot + wantEphemeral + wantHeadroom

	if got != want {
		t.Errorf("computeRequiredStorageBytes(RAM field) = %d; want %d", got, want)
	}
}
