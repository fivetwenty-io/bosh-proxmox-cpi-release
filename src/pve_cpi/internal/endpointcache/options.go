package endpointcache

import "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"

// DirOptions is everything the cache directory layers read from the CPI
// config. Every caller that chooses the directory builds it with OptionsFor
// and chooses through its Resolve method, so a new layer reaches all of them at
// once and no caller names a config field itself.
type DirOptions struct {
	// Override is pve.endpoint_cache_dir, empty when it is unset.
	Override string

	// JournalDir is pve.storage_allocation_journal_dir, empty when it is
	// unset. Options has no journal field yet, so no layer reads this value
	// today. It is carried here so that the layer beside the journal, which
	// adds that field, takes it from this struct in Options and every caller
	// gets it without a change.
	JournalDir string
}

// OptionsFor builds the directory options from the config. A nil config gives
// the options of an unconfigured CPI, which choose the temporary layer.
func OptionsFor(cfg *config.CPIConfig) DirOptions {
	if cfg == nil {
		return DirOptions{}
	}
	return DirOptions{Override: cfg.EndpointCacheDir, JournalDir: cfg.StorageAllocationJournalDir}
}

// Options returns the options Resolve takes. It is the one place that copies
// the config's values into them.
func (d DirOptions) Options() Options { return Options{Override: d.Override} }

// Resolve chooses the cache directory for d. See the package's Resolve.
func (d DirOptions) Resolve() Choice { return Resolve(d.Options()) }

// WithoutOverride returns d with pve.endpoint_cache_dir unset, so a caller that
// may fall back from a missing override reaches the layers after it.
func (d DirOptions) WithoutOverride() DirOptions {
	d.Override = ""
	return d
}
