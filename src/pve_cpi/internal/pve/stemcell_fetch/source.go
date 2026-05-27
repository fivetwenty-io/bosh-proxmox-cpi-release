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
// "stemcell_fetch". See implementation notes in the workspace for rationale.
package stemcellfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Reference identifies a remote stemcell source after URL parsing. Each
// source populates only the fields relevant to its scheme.
type Reference struct {
	Scheme string // "https", "s3", "bosh+blobstore", "oci"
	URL    string // original URL string, preserved for logging/error messages

	// HTTPS / generic
	Host string
	Path string

	// S3
	Bucket string
	Key    string

	// BOSH blobstore
	BlobID string

	// OCI
	Image string // registry/repo
	Tag   string
}

// Source streams a remote stemcell qcow2 to the caller. Implementations
// return an io.ReadCloser the caller must drain and close. ContentLength is
// -1 when the protocol cannot report size up front (chunked or OCI layered).
type Source interface {
	// Fetch opens a streaming read of the remote stemcell. Caller must close
	// the returned body. On error, body is nil and no cleanup is required.
	Fetch(ctx context.Context, ref Reference, creds Credentials) (body io.ReadCloser, contentLength int64, err error)
}

// ResolveSource inspects rawURL's scheme and returns the matching Source
// along with a populated Reference. https://, s3://, bosh+blobstore:, and
// oci:// are all wired.
//
// Error conditions:
//   - empty rawURL → error
//   - unknown/unsupported scheme → error with list of supported schemes
func ResolveSource(rawURL string) (Source, Reference, error) {
	if rawURL == "" {
		return nil, Reference{}, fmt.Errorf("stemcell_fetch: image_url is empty")
	}

	ref := Reference{URL: rawURL}

	switch {
	case strings.HasPrefix(rawURL, "https://"):
		ref.Scheme = "https"
		return newHTTPSSource(), ref, nil

	case strings.HasPrefix(rawURL, "s3://"):
		ref.Scheme = "s3"
		bucket, key, err := parseS3URL(rawURL)
		if err != nil {
			return nil, ref, err
		}
		ref.Bucket = bucket
		ref.Key = key
		return newS3Source(), ref, nil

	case strings.HasPrefix(rawURL, "bosh+blobstore:"):
		ref.Scheme = "bosh+blobstore"
		// BlobID is everything after the scheme prefix; no "//" authority.
		ref.BlobID = strings.TrimPrefix(rawURL, "bosh+blobstore:")
		if ref.BlobID == "" {
			return nil, ref, fmt.Errorf("stemcell_fetch: bosh+blobstore URL has empty blob id (got %q)", rawURL)
		}
		return newBlobstoreSource(), ref, nil

	case strings.HasPrefix(rawURL, "oci://"):
		ref.Scheme = "oci"
		rest := strings.TrimPrefix(rawURL, "oci://")
		// image may be "host/repo" with tag separated by the last ":" that has
		// no "/" after it (the colon is not part of a port number).
		if i := strings.LastIndex(rest, ":"); i > 0 && !strings.Contains(rest[i:], "/") {
			ref.Image = rest[:i]
			ref.Tag = rest[i+1:]
		} else {
			ref.Image = rest
			ref.Tag = "latest"
		}
		return newOCISource(), ref, nil

	default:
		return nil, ref, fmt.Errorf(
			"stemcell_fetch: unsupported URL scheme in %q (supported: https://, s3://, bosh+blobstore:, oci://): %w",
			rawURL, errNotImplemented,
		)
	}
}

// errNotImplemented is the sentinel for a known-but-not-yet-wired scheme.
// Callers that need to distinguish "not-yet-implemented" from "unsupported
// scheme" may use errors.Is against this value.
var errNotImplemented = errors.New("stemcell_fetch: source not yet implemented")
