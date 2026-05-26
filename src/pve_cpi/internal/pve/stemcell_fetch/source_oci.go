package stemcellfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ociSource implements Source for oci:// references. Pulls the artifact's
// manifest from a registry, finds the qcow2 layer (by media-type or fallback
// heuristics), and streams the layer's blob uncompressed.
//
// Failure modes handled by Fetch:
//   - non-oci:// URL → error from parseOCIRef
//   - wrong/incompatible Credentials type → error
//   - malformed image reference → wrapped error from go-containerregistry/pkg/name
//   - network / auth error on pull → wrapped error from remote.Image
//   - image with no layers → explicit error
//   - layer blob open failure → wrapped error from layer.Uncompressed
type ociSource struct{}

func newOCISource() *ociSource { return &ociSource{} }

// ociCredentials is the concrete Credentials type for type="oci" auth payloads.
// Apply is a no-op: the OCI source uses go-containerregistry authn.Basic, not
// raw HTTP header injection.
//
// JSON fields:
//
//	{"type":"oci","username":"robot$cpi","password":"..."}
//
// username and password are optional — registries supporting anonymous pulls
// do not require credentials.
type ociCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (ociCredentials) Apply(_ *http.Request) error { return nil }
func (ociCredentials) Kind() string                { return "oci" }

// parseOCIAuth deserializes raw into ociCredentials.
//
// Failure modes:
//   - malformed JSON → wrapped unmarshal error
//   - username/password absent → valid (anonymous pull)
func parseOCIAuth(raw json.RawMessage) (ociCredentials, error) {
	var c ociCredentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("stemcell_fetch(oci): parse credentials: %w", err)
	}
	// username/password optional — anonymous pull is valid for public registries.
	return c, nil
}

// stemcellQcow2MediaType is the conventional OCI artifact layer media-type
// the CPI publishes for stemcell qcow2 contents. Operators may upload with
// other media-types; the source falls back to "first non-standard-rootfs layer"
// if no media-type match is found, and finally to the first layer.
const stemcellQcow2MediaType = "application/vnd.bosh.stemcell.qcow2"

// parseOCIRef strips the oci:// prefix and returns the bare registry/repo[:tag]
// string that go-containerregistry's name.ParseReference accepts.
//
// Failure modes:
//   - missing oci:// prefix → error
//   - empty string after prefix → error
func parseOCIRef(rawURL string) (string, error) {
	if !strings.HasPrefix(rawURL, "oci://") {
		return "", fmt.Errorf("stemcell_fetch(oci): URL %q missing oci:// prefix", rawURL)
	}
	rest := strings.TrimPrefix(rawURL, "oci://")
	if rest == "" {
		return "", fmt.Errorf("stemcell_fetch(oci): URL %q empty after prefix", rawURL)
	}
	return rest, nil
}

// Fetch opens a streaming read of the qcow2 layer from the OCI image at
// ref.URL. The returned body must be drained and closed by the caller.
// contentLength is the compressed layer size from the manifest; may be -1 if
// the registry omits it (non-standard registries).
//
// creds must be one of:
//   - ociCredentials — concrete type from parseAuth / parseOCIAuth
//   - rawAuthCreds — raw payload arriving before this source was wired; decoded here
//   - noCreds — anonymous pull; valid for public registries
//
// Any other Credentials implementation is rejected with an incompatible-kind error.
//
// Layer selection strategy (in priority order):
//  1. First layer whose MediaType() == stemcellQcow2MediaType
//  2. First layer that is not a standard Docker/OCI rootfs tar layer
//  3. First layer (final fallback for plain single-layer images)
func (o *ociSource) Fetch(ctx context.Context, ref Reference, creds Credentials) (io.ReadCloser, int64, error) {
	refStr, err := parseOCIRef(ref.URL)
	if err != nil {
		return nil, 0, err
	}

	authenticator, err := resolveOCIAuthn(creds)
	if err != nil {
		return nil, 0, err
	}

	parsedRef, err := name.ParseReference(refStr)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(oci): parse ref %q: %w", refStr, err)
	}

	img, err := remote.Image(parsedRef, remote.WithAuth(authenticator), remote.WithContext(ctx))
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(oci): pull image %q: %w", refStr, err)
	}

	v1Layers, err := img.Layers()
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(oci): read layers from %q: %w", refStr, err)
	}
	if len(v1Layers) == 0 {
		return nil, 0, fmt.Errorf("stemcell_fetch(oci): image %q has no layers", refStr)
	}
	layers := make([]ociLayer, len(v1Layers))
	for i, l := range v1Layers {
		layers[i] = l
	}

	picked := pickLayer(layers)

	size, err := picked.Size()
	if err != nil {
		// Non-fatal: size may be unavailable from non-standard registries.
		// Return -1 so callers can detect "unknown content-length".
		size = -1
	}

	body, err := picked.Uncompressed()
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(oci): open layer blob: %w", err)
	}
	return body, size, nil
}

// resolveOCIAuthn converts a Credentials value into an authn.Authenticator
// understood by go-containerregistry's remote package.
//
// Failure modes:
//   - rawAuthCreds with malformed JSON → wrapped error from parseOCIAuth
//   - incompatible Credentials kind → error with kind string
func resolveOCIAuthn(creds Credentials) (authn.Authenticator, error) {
	switch v := creds.(type) {
	case ociCredentials:
		if v.Username != "" || v.Password != "" {
			return &authn.Basic{Username: v.Username, Password: v.Password}, nil
		}
		return authn.Anonymous, nil

	case rawAuthCreds:
		// Payload arrived via the generic parseAuth path. Decode and recurse.
		oc, err := parseOCIAuth(v.Raw)
		if err != nil {
			return nil, err
		}
		return resolveOCIAuthn(oc)

	case noCreds:
		return authn.Anonymous, nil

	default:
		return nil, fmt.Errorf("stemcell_fetch(oci): incompatible credentials kind %q", creds.Kind())
	}
}

// pickLayer selects the most appropriate layer from a non-empty slice.
// See Fetch doc for selection strategy.
func pickLayer(layers []ociLayer) ociLayer {
	// Standard rootfs layer media-types — skip these when a better candidate exists.
	isStandardRootfs := func(mt types.MediaType) bool {
		return mt == types.DockerLayer ||
			mt == types.OCILayer ||
			mt == types.OCILayerZStd ||
			mt == types.DockerUncompressedLayer ||
			mt == types.OCIUncompressedLayer
	}

	var fallback ociLayer // first non-standard-rootfs layer seen
	for _, l := range layers {
		mt, err := l.MediaType()
		if err != nil {
			// Layer with unreadable media-type: skip for preference matching,
			// keep as candidate if no better match exists.
			if fallback == nil {
				fallback = l
			}
			continue
		}
		if types.MediaType(mt) == types.MediaType(stemcellQcow2MediaType) {
			return l
		}
		if !isStandardRootfs(mt) && fallback == nil {
			fallback = l
		}
	}
	if fallback != nil {
		return fallback
	}
	return layers[0]
}

// ociLayer is a subset of v1.Layer used internally so tests can inject fakes
// without importing the full go-containerregistry v1 package.
type ociLayer interface {
	MediaType() (types.MediaType, error)
	Size() (int64, error)
	Uncompressed() (io.ReadCloser, error)
}
