// Credential kind and URL-scheme string identifiers used across the stemcell
// fetch package. Centralised so call sites share a single source of truth.

package stemcellfetch

// Credential kind tags returned by Credentials.Kind() and accepted by
// ParseCredentials.
const (
	credKindBasic     = "basic"
	credKindBearer    = "bearer"
	credKindBlobstore = "blobstore"
	credKindOCI       = "oci"
)

// URL-scheme tags recognised by ResolveSource for stemcell-fetch references.
const (
	schemeHTTPS         = "https"
	schemeBOSHBlobstore = "bosh+blobstore"
	schemeOCI           = "oci"
)
