package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// cmNone is the PVE "none" cache mode, factored to a const so repeated test
// cases do not trip the goconst occurrence threshold.
const cmNone = "none"

// TestDiskPerfInvariantViolations covers the pure invariant-divergence detector
// used by §7.26 enforcement. Absence of an option is treated as a value: an
// option present at attach but absent at creation (or vice versa, or changed)
// is a divergence for the structural keys {cache,iothread,ssd,aio}; throttle
// knobs and discard are never invariants.
func TestDiskPerfInvariantViolations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		creation       map[string]string
		effective      map[string]string
		wantViolations []string // substrings expected, one per violation
	}{
		{
			name:      "identical structural opts → no violation",
			creation:  map[string]string{"cache": "writeback", "iothread": "1"},
			effective: map[string]string{"cache": "writeback", "iothread": "1"},
		},
		{
			name:      "global introduces cache absent at creation → cache violation",
			creation:  map[string]string{"iothread": "1"},
			effective: map[string]string{"iothread": "1", "cache": "writeback"},
			wantViolations: []string{
				"cache: created with (unset), attach would apply \"writeback\"",
			},
		},
		{
			name:      "global introduces iothread absent at creation → iothread violation",
			creation:  map[string]string{"cache": "writeback"},
			effective: map[string]string{"cache": "writeback", "iothread": "1"},
			wantViolations: []string{
				"iothread: created with (unset), attach would apply \"1\"",
			},
		},
		{
			name:      "ssd introduced → ssd violation",
			creation:  map[string]string{"cache": cmNone},
			effective: map[string]string{"cache": cmNone, "ssd": "1"},
			wantViolations: []string{
				"ssd: created with (unset), attach would apply \"1\"",
			},
		},
		{
			name:      "aio introduced → aio violation",
			creation:  map[string]string{"cache": cmNone},
			effective: map[string]string{"cache": cmNone, "aio": "native"},
			wantViolations: []string{
				"aio: created with (unset), attach would apply \"native\"",
			},
		},
		{
			name:      "changed aio value → aio violation",
			creation:  map[string]string{"aio": "native"},
			effective: map[string]string{"aio": "io_uring"},
			wantViolations: []string{
				"aio: created with \"native\", attach would apply \"io_uring\"",
			},
		},
		{
			name:      "changed cache value → cache violation",
			creation:  map[string]string{"cache": "writeback"},
			effective: map[string]string{"cache": cmNone},
			wantViolations: []string{
				"cache: created with \"writeback\", attach would apply \"none\"",
			},
		},
		{
			name:      "creation has invariant dropped at attach → violation",
			creation:  map[string]string{"cache": "writeback", "iothread": "1"},
			effective: map[string]string{"cache": "writeback"},
			wantViolations: []string{
				"iothread: created with \"1\", attach would apply (unset)",
			},
		},
		{
			name:      "throttle drift is NOT an invariant → no violation",
			creation:  map[string]string{"cache": cmNone, "mbps_rd": "50", "iops_wr": "100"},
			effective: map[string]string{"cache": cmNone, "mbps_rd": "200", "iops_wr": "9000"},
		},
		{
			name:      "discard drift is NOT an invariant → no violation",
			creation:  map[string]string{"cache": cmNone},
			effective: map[string]string{"cache": cmNone, "discard": "on"},
		},
		{
			name:      "empty creation and effective → no violation",
			creation:  map[string]string{},
			effective: map[string]string{},
		},
		{
			name:      "multiple structural divergences → multiple violations",
			creation:  map[string]string{},
			effective: map[string]string{"cache": "writeback", "iothread": "1", "ssd": "1"},
			wantViolations: []string{
				"cache: created with (unset), attach would apply \"writeback\"",
				"iothread: created with (unset), attach would apply \"1\"",
				"ssd: created with (unset), attach would apply \"1\"",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := diskPerfInvariantViolations(c.creation, c.effective)
			if len(got) != len(c.wantViolations) {
				t.Fatalf("violation count: got %d %v, want %d %v",
					len(got), got, len(c.wantViolations), c.wantViolations)
			}
			joined := strings.Join(got, "\n")
			for _, want := range c.wantViolations {
				if !strings.Contains(joined, want) {
					t.Errorf("missing expected violation %q in:\n%s", want, joined)
				}
			}
		})
	}
}

// TestEnforceDiskPerfInvariants_Gate covers the opt-in gate and nil-config
// safety of the handler-side enforcement wrapper directly (the pure detector is
// covered by TestDiskPerfInvariantViolations).
func TestEnforceDiskPerfInvariants_Gate(t *testing.T) {
	t.Parallel()
	logger := log.NewNopLogger()
	// A divergence: created with no cache, attach would apply cache=writeback.
	diverging := map[string]string{"cache": "writeback"}

	t.Run("nil meta → skip (no error)", func(t *testing.T) {
		t.Parallel()
		cfg := &config.CPIConfig{} // empty → enforce
		if err := enforceDiskPerfInvariants(cfg, logger, "attach_disk", "100", "cid", nil, diverging); err != nil {
			t.Errorf("nil meta must skip, got: %v", err)
		}
	})

	t.Run("meta with empty Opts → skip (no error)", func(t *testing.T) {
		t.Parallel()
		cfg := &config.CPIConfig{}
		meta := &pve.DiskCIDMeta{Opts: map[string]string{}}
		if err := enforceDiskPerfInvariants(cfg, logger, "attach_disk", "100", "cid", meta, diverging); err != nil {
			t.Errorf("empty Opts must skip, got: %v", err)
		}
	})

	t.Run("nil config + divergence → fail-closed enforce", func(t *testing.T) {
		t.Parallel()
		meta := &pve.DiskCIDMeta{Opts: map[string]string{"iothread": "1"}}
		// effective adds cache=writeback (a structural key absent at creation).
		effective := map[string]string{"iothread": "1", "cache": "writeback"}
		err := enforceDiskPerfInvariants(nil, logger, "attach_disk", "100", "cid", meta, effective)
		if err == nil {
			t.Fatal("nil config + divergence must reject (enforce default), got nil")
		}
		if !cpierrors.IsType(err, cpierrors.TypeCloud) {
			t.Errorf("want TypeCloud, got: %v", err)
		}
	})
}

// TestDevicePathByID_BasicSlots verifies that diskID "scsi<N>" maps to the
// PVE-stable udev by-id symlink path. PVE configures virtio-scsi-pci disks
// with serial "drive-scsi<N>" and udev creates a matching by-id link.
func TestDevicePathByID_BasicSlots(t *testing.T) {
	t.Parallel()

	cases := []struct {
		diskID, want string
	}{
		{"scsi0", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0"},
		{"scsi1", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"},
		{"scsi3", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi3"},
		{"scsi30", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi30"},
	}

	for _, c := range cases {
		t.Run(c.diskID, func(t *testing.T) {
			t.Parallel()
			got, err := devicePathByID(c.diskID)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestDevicePathByID_NonSCSIRejected verifies that non-scsi diskIDs cause
// an explicit error. attach_disk asserts the bus is "scsi" before calling
// this helper, but the guard prevents silent misuse if that contract
// changes.
func TestDevicePathByID_NonSCSIRejected(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, diskID string }{
		{"virtio", "virtio0"},
		{"ide", "ide0"},
		{"sata", "sata0"},
		{"empty", ""},
		{"garbage", "garbage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := devicePathByID(c.diskID); err == nil {
				t.Errorf("devicePathByID(%q) succeeded, expected error", c.diskID)
			}
		})
	}
}
