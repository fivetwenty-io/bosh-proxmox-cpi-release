package stemcellfetch

import (
	"context"
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

// ResolveSourceWith inspects rawURL's scheme and returns the matching Source
// along with a populated Reference. https://, s3://, bosh+blobstore:, and
// oci:// are all wired. Constructs sources whose HTTP clients honor the
// caller-supplied TransportConfig (applies only to the https and
// bosh+blobstore sources; s3 and oci use their own SDK clients). Used by
// production callers that thread operator-tunable timeouts from the
// CPI config.
//
// Error conditions:
//   - empty rawURL → error
//   - unknown/unsupported scheme → error with list of supported schemes
func ResolveSourceWith(rawURL string, tc TransportConfig) (Source, Reference, error) {
	if rawURL == "" {
		return nil, Reference{}, fmt.Errorf("stemcell_fetch: image_url is empty")
	}

	ref := Reference{URL: rawURL}

	switch {
	case strings.HasPrefix(rawURL, "https://"):
		ref.Scheme = schemeHTTPS
		return newHTTPSSource(tc), ref, nil

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
		ref.Scheme = schemeBOSHBlobstore
		// BlobID is everything after the scheme prefix; no "//" authority.
		ref.BlobID = strings.TrimPrefix(rawURL, "bosh+blobstore:")
		if ref.BlobID == "" {
			return nil, ref, fmt.Errorf("stemcell_fetch: bosh+blobstore URL has empty blob id (got %q)", rawURL)
		}
		return newBlobstoreSource(tc), ref, nil

	case strings.HasPrefix(rawURL, "oci://"):
		ref.Scheme = schemeOCI
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
			"stemcell_fetch: unsupported URL scheme in %q (supported: https://, s3://, bosh+blobstore:, oci://)",
			rawURL,
		)
	}
}
