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
// config → empty map (byte-identical: caller emits nothing).
//
// Mapping:
//
//	iothread bool → "1"/omit (false or absent → omit; we never emit "0")
//	ssd      bool → "1"/omit
//	discard  bool → "on"/omit (true) / omit (false or absent)
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
	if resolveDiskPerfBool(r, diskOptIothread, diskPerfCfgBool(dp, func(d *config.DiskPerformanceDefaults) *bool { return d.Iothread })) {
		opts[diskOptIothread] = "1"
	}
	if resolveDiskPerfBool(r, diskOptSSD, diskPerfCfgBool(dp, func(d *config.DiskPerformanceDefaults) *bool { return d.SSD })) {
		opts[diskOptSSD] = "1"
	}
	if resolveDiskPerfBool(r, "discard", diskPerfCfgBool(dp, func(d *config.DiskPerformanceDefaults) *bool { return d.Discard })) {
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
// false counts), else the config pointer, else false.
func resolveDiskPerfBool(r *layeredResolver, key string, cfgVal *bool) bool {
	if v, found := r.Bool(key); found {
		return v
	}
	if cfgVal != nil {
		return *cfgVal
	}
	return false
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
// (opt-in). Precedence:
//
//  1. resolver Bool "virtio_scsi_single" (if found) — explicit call value wins
//  2. cfg.DiskPerformance.VirtioSCSISingle (if non-nil) — global config default
//  3. false
func resolveVirtioSCSISingle(r *layeredResolver, cfg *config.CPIConfig) bool {
	if v, found := r.Bool("virtio_scsi_single"); found {
		return v
	}
	if cfg != nil && cfg.DiskPerformance != nil && cfg.DiskPerformance.VirtioSCSISingle != nil {
		return *cfg.DiskPerformance.VirtioSCSISingle
	}
	return false
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
