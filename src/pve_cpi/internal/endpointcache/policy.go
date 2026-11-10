package endpointcache

import "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"

// cacheLabel prefixes the fixed error texts of the private-file helpers.
const cacheLabel = "endpoint cache"

// The unsafe path causes carry this package's wording and match the
// privatefile causes under errors.Is.
var (
	errCacheOwnership = privatefile.Sentinel("endpoint cache: unsafe ownership", privatefile.ErrUnsafeOwnership)
	errCacheMode      = privatefile.Sentinel("endpoint cache: unsafe permissions", privatefile.ErrUnsafeMode)
	errCacheFileType  = privatefile.Sentinel("endpoint cache: unsafe file type", privatefile.ErrUnsafeFileType)
)

// cachePrivatePolicy carries this package's label and unsafe path causes. The
// cache directory, its lock files, the directory secret, and the cache files
// are all checked through it, so each refusal reads the same whichever file
// met it. The secret and cache file policies extend it with their size limits
// and checks.
var cachePrivatePolicy = privatefile.Policy{
	Label:     cacheLabel,
	Ownership: errCacheOwnership,
	Mode:      errCacheMode,
	FileType:  errCacheFileType,
}
