package handlers

import (
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// resolveDiskPerfOptions resolves PVE per-disk performance options from the
// layered resolver (call→disk_type→vm_type per §7.8), falling back per-key to
// cfg.DiskPerformance global defaults. Returns a PVE option map (key→value
// string) containing only options that were set. Non-retriable CloudError on
// invalid value (bad cache mode, negative throttle). Empty resolver + nil/empty
// config → empty map, EXCEPT iothread — see below.
//
// Mapping:
//
//	iothread bool → "1"/omit. Default TRUE (Phase 2): a dedicated I/O thread
//	                per disk relieves QEMU main-loop contention on multi-disk
//	                VMs and is the modern PVE creation default; this default
//	                applies only when neither the layered resolver
//	                (cloud_properties.iothread, including a vm_type/disk_type
//	                profile) nor cfg.DiskPerformance.Iothread set an explicit
//	                value — an explicit false at either layer still disables
//	                it. This is a create/attach-time bake only: existing disk
//	                CIDs and already-created VMs are unaffected (see
//	                disk_perf_invariant_mode for re-attach drift handling).
//	ssd      bool → "1"/omit. Default false (unchanged).
//	discard  bool → "on"/omit (true) / omit (false or absent). Default false
//	                (unchanged).
//	cache    string (validated) → opts["cache"]=mode / omit when empty
//	mbps_rd  float → strconv.FormatFloat(v,'g',-1,64) / omit when 0; <0 → error
//	mbps_wr  same pattern
//	iops_rd  int → strconv.Itoa(v) / omit when 0; <0 → error
//	iops_wr  same pattern
func resolveDiskPerfOptions(r *layeredResolver, cfg *config.CPIConfig) (map[string]string, error) {
	opts := make(map[string]string)

	var dp *config.DiskPerformanceDefaults
	if cfg != nil {
		dp = cfg.DiskPerformance
	}

	// Boolean toggles: emit only when effective value is true. false/absent → omit.
	// iothread's built-in default is true (Phase 2 default flip); ssd/discard
	// stay false — see resolveDiskPerfOptions doc for the full rationale.
	if resolveDiskPerfBool(r, diskOptIothread, diskPerfCfgBool(dp, func(d *config.DiskPerformanceDefaults) *bool { return d.Iothread }), true) {
		opts[diskOptIothread] = "1"
	}
	if resolveDiskPerfBool(r, diskOptSSD, diskPerfCfgBool(dp, func(d *config.DiskPerformanceDefaults) *bool { return d.SSD }), false) {
		opts[diskOptSSD] = "1"
	}
	if resolveDiskPerfBool(r, "discard", diskPerfCfgBool(dp, func(d *config.DiskPerformanceDefaults) *bool { return d.Discard }), false) {
		opts["discard"] = "on"
	}

	// cache: validated string. resolver value wins; else config default.
	cacheMode, ok := r.String(diskOptCache)
	if !ok && dp != nil {
		cacheMode = dp.Cache
	}
	if err := validateDiskPerfCache(cacheMode); err != nil {
		return nil, err
	}
	if cacheMode != "" {
		opts[diskOptCache] = cacheMode
	}

	// Throttles: emit only when > 0; < 0 → error.
	if err := resolveDiskPerfFloatOpt(opts, r, "mbps_rd", diskPerfCfgFloat(dp, func(d *config.DiskPerformanceDefaults) *float64 { return d.MBpsRd })); err != nil {
		return nil, err
	}
	if err := resolveDiskPerfFloatOpt(opts, r, "mbps_wr", diskPerfCfgFloat(dp, func(d *config.DiskPerformanceDefaults) *float64 { return d.MBpsWr })); err != nil {
		return nil, err
	}
	if err := resolveDiskPerfIntOpt(opts, r, "iops_rd", diskPerfCfgInt(dp, func(d *config.DiskPerformanceDefaults) *int { return d.IOPSRd })); err != nil {
		return nil, err
	}
	if err := resolveDiskPerfIntOpt(opts, r, "iops_wr", diskPerfCfgInt(dp, func(d *config.DiskPerformanceDefaults) *int { return d.IOPSWr })); err != nil {
		return nil, err
	}

	return opts, nil
}

// diskPerfCfgBool/Float/Int safely extract a pointer field from a possibly-nil
// DiskPerformanceDefaults. Returns nil when dp is nil.
func diskPerfCfgBool(dp *config.DiskPerformanceDefaults, get func(*config.DiskPerformanceDefaults) *bool) *bool {
	if dp == nil {
		return nil
	}
	return get(dp)
}

func diskPerfCfgFloat(dp *config.DiskPerformanceDefaults, get func(*config.DiskPerformanceDefaults) *float64) *float64 {
	if dp == nil {
		return nil
	}
	return get(dp)
}

func diskPerfCfgInt(dp *config.DiskPerformanceDefaults, get func(*config.DiskPerformanceDefaults) *int) *int {
	if dp == nil {
		return nil
	}
	return get(dp)
}

// resolveDiskPerfBool returns the effective bool: resolver value wins (explicit
// false counts), else the config pointer (explicit false also counts), else
// defaultVal. defaultVal lets callers give a specific option (iothread) a
// true built-in default while others (ssd, discard) keep false — both an
// explicit resolver-level and an explicit config-level false still disable
// the option regardless of defaultVal, since either non-absent source is
// checked before defaultVal is ever consulted.
func resolveDiskPerfBool(r *layeredResolver, key string, cfgVal *bool, defaultVal bool) bool {
	if v, found := r.Bool(key); found {
		return v
	}
	if cfgVal != nil {
		return *cfgVal
	}
	return defaultVal
}

// resolveDiskPerfFloatOpt resolves a float throttle and, when > 0, writes it to
// opts[key] formatted with %g. resolver value wins; else config pointer. A
// negative value (call or config) is a non-retriable CloudError.
func resolveDiskPerfFloatOpt(opts map[string]string, r *layeredResolver, key string, cfgVal *float64) error {
	v, present := r.Float(key)
	if !present {
		if cfgVal == nil {
			return nil
		}
		v = *cfgVal
	}
	if v < 0 {
		return cpierrors.Cloud("disk_performance: %s must be >= 0, got %g", key, v)
	}
	if v > 0 {
		opts[key] = strconv.FormatFloat(v, 'g', -1, 64)
	}
	return nil
}

// resolveDiskPerfIntOpt resolves an integer throttle and, when > 0, writes it to
// opts[key]. resolver value wins; else config pointer. Negative → CloudError.
func resolveDiskPerfIntOpt(opts map[string]string, r *layeredResolver, key string, cfgVal *int) error {
	v, present := r.Int(key)
	if !present {
		if cfgVal == nil {
			return nil
		}
		v = *cfgVal
	}
	if v < 0 {
		return cpierrors.Cloud("disk_performance: %s must be >= 0, got %d", key, v)
	}
	if v > 0 {
		opts[key] = strconv.Itoa(v)
	}
	return nil
}

// filterDiskPerfForBus returns a copy of opts with bus-invalid keys removed.
//
//   - "virtio" drops "ssd" (virtio-blk has no rotation-rate flag)
//   - "scsi", "sata", "ide" keep all keys
//   - Unknown bus keeps all keys
//
// The input map is not mutated. nil or empty input returns an empty map.
func filterDiskPerfForBus(opts map[string]string, bus string) map[string]string {
	out := make(map[string]string, len(opts))
	for k, v := range opts {
		out[k] = v
	}
	if bus == "virtio" {
		delete(out, "ssd")
	}
	return out
}

// resolveVirtioSCSISingle reports whether the VM should use virtio-scsi-single
// (default as of Phase 2 — see below). Precedence:
//
//  1. resolver Bool "virtio_scsi_single" (if found) — explicit call value wins,
//     including an explicit false from cloud_properties or a vm_type/disk_type
//     profile (disables the default).
//  2. cfg.DiskPerformance.VirtioSCSISingle (if non-nil) — global config default;
//     an explicit *false here also disables the built-in default below.
//  3. true — the built-in default. VirtIO SCSI single gives each disk its own
//     dedicated controller (required for per-disk iothread on the scsi bus,
//     whose default also flipped to true in the same change) instead of
//     serializing every disk behind one shared virtio-scsi-pci controller.
//     This is create-time only: an existing VM's controller is never changed
//     retroactively. Set virtio_scsi_single: false (globally or per-call) to
//     restore the pre-Phase-2 virtio-scsi-pci default.
func resolveVirtioSCSISingle(r *layeredResolver, cfg *config.CPIConfig) bool {
	if v, found := r.Bool("virtio_scsi_single"); found {
		return v
	}
	if cfg != nil && cfg.DiskPerformance != nil && cfg.DiskPerformance.VirtioSCSISingle != nil {
		return *cfg.DiskPerformance.VirtioSCSISingle
	}
	return true
}

// diskPerfInvariantKeys are the structural per-disk performance options whose
// value is fixed at create_disk time and must not change on re-attach. Throttle
// knobs (mbps_*, iops_*) and discard are deliberately excluded: PVE can change
// them on a live device without a structural reconfiguration, so they are not
// invariants.
//
//	cache    — host I/O caching mode; changing it on a live device risks
//	           in-flight write semantics (Azure treats caching as immutable).
//	iothread — dedicated I/O thread; toggling requires controller reconfiguration.
//	ssd      — SSD emulation flag; affects guest discard/trim behavior.
var diskPerfInvariantKeys = []string{diskOptCache, diskOptIothread, diskOptSSD}

// diskPerfInvariantViolations compares the creation-time disk-performance
// options recorded in the disk CID against the effective options that would be
// applied at attach_disk time, and returns a human-readable description for each
// structural invariant (see diskPerfInvariantKeys) that diverges. Absence is a
// value: an option present at attach but absent at creation (e.g. a global
// default introduced after the disk was created) is a divergence, as is the
// reverse or a changed value.
//
// An empty result means no invariant diverged. The function is pure — the caller
// decides whether a non-empty result is an error, a warning, or ignored, per the
// disk_perf_invariant_mode config knob.
func diskPerfInvariantViolations(creationOpts, effectiveOpts map[string]string) []string {
	var violations []string
	for _, key := range diskPerfInvariantKeys {
		created, hasCreated := creationOpts[key]
		effective, hasEffective := effectiveOpts[key]
		if hasCreated == hasEffective && created == effective {
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"%s: created with %s, attach would apply %s",
			key,
			diskPerfOptDisplay(created, hasCreated),
			diskPerfOptDisplay(effective, hasEffective),
		))
	}
	return violations
}

// diskPerfOptDisplay renders an option value for a divergence message, marking
// an absent option as "(unset)" so the distinction between "unset" and an
// explicit value is unambiguous.
func diskPerfOptDisplay(value string, present bool) string {
	if !present {
		return "(unset)"
	}
	return strconv.Quote(value)
}

// validateDiskPerfCache returns a non-retriable CloudError if mode is non-empty
// and not a known PVE cache mode. Empty string is valid (means "no override").
// Delegates to config.IsKnownDiskCacheMode so the accepted set lives in one place.
func validateDiskPerfCache(mode string) error {
	if config.IsKnownDiskCacheMode(mode) {
		return nil
	}
	return cpierrors.Cloud(
		"disk_performance: cache must be one of none|writethrough|writeback|unsafe|directsync, got %q",
		mode,
	)
}
