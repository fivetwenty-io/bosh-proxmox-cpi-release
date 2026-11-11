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
	// unset. Options carries it to the layer beside the journal, so every
	// caller that builds its options with OptionsFor gets that layer.
	JournalDir string

	// DisplayName is what a refusal calls the cache the caller is choosing a
	// directory for. Empty means the endpoint cache. OptionsFor leaves it empty,
	// and Named sets it.
	DisplayName string

	// Guard is Options.Guard. OptionsFor leaves it nil, and WithGuard sets it.
	Guard PathGuard
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
func (d DirOptions) Options() Options {
	return Options(d)
}

// Resolve chooses the cache directory for d. See the package's Resolve.
func (d DirOptions) Resolve() Choice { return Resolve(d.Options()) }

// WithoutOverride returns d with pve.endpoint_cache_dir unset, so a caller that
// may fall back from a missing override reaches the layers after it.
func (d DirOptions) WithoutOverride() DirOptions {
	d.Override = ""
	return d
}

// Named returns d with refusals calling the cache name, such as HAProfileName.
func (d DirOptions) Named(name string) DirOptions {
	d.DisplayName = name
	return d
}

// WithGuard returns d with guard vetting the path of every layer that creates
// a directory, before it creates it.
func (d DirOptions) WithGuard(guard PathGuard) DirOptions {
	d.Guard = guard
	return d
}
