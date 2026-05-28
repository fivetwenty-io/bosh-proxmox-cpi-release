// Package stemcellfetch implements CPI-assisted-fetch for light stemcells.
//
// Operators reference a remote qcow2 via cloud_properties.image_url. The
// fetch package resolves the URL to a Source implementation, streams the
// image through SHA-256 hashing into the PVE Upload API, and returns a
// canonical filename + sha8 for the resulting light CID.
//
// Sources are pluggable behind a single Source interface. Each scheme
// (https, s3, bosh+blobstore, oci) has one file implementing Source.
//
// Package name: Go convention disallows underscores in package identifiers,
// so the package is named "stemcellfetch" while the directory is named
// "stemcell_fetch".
package stemcellfetch
