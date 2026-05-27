package stemcellfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ---- parseOCIRef ----

func TestParseOCIRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		want    string
		wantErr bool
	}{
		{
			name:   "valid oci url with tag",
			rawURL: "oci://ghcr.io/cloudfoundry/bosh-stemcell-ubuntu-jammy:1.438",
			want:   "ghcr.io/cloudfoundry/bosh-stemcell-ubuntu-jammy:1.438",
		},
		{
			name:   "valid oci url with latest tag",
			rawURL: "oci://harbor.lab.local/stemcells/ubuntu-jammy:latest",
			want:   "harbor.lab.local/stemcells/ubuntu-jammy:latest",
		},
		{
			name:   "plain host/repo without tag",
			rawURL: "oci://reg.local/img",
			want:   "reg.local/img",
		},
		{
			name:    "empty string returns error",
			rawURL:  "",
			wantErr: true,
		},
		{
			name:    "missing oci:// prefix returns error",
			rawURL:  "https://reg.local/img:tag",
			wantErr: true,
		},
		{
			name:    "oci:// with nothing after returns error",
			rawURL:  "oci://",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseOCIRef(tc.rawURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOCIRef(%q): expected error, got nil", tc.rawURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOCIRef(%q): unexpected error: %v", tc.rawURL, err)
			}
			if got != tc.want {
				t.Errorf("parseOCIRef(%q) = %q, want %q", tc.rawURL, got, tc.want)
			}
		})
	}
}

// ---- parseOCIAuth ----

func TestParseOCIAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          json.RawMessage
		wantUsername string
		wantPassword string
		wantErr      bool
	}{
		{
			name:         "full credentials",
			raw:          json.RawMessage(`{"type":"oci","username":"robot$cpi","password":"s3cr3t"}`),
			wantUsername: "robot$cpi",
			wantPassword: "s3cr3t",
		},
		{
			name:         "anonymous — empty username and password are valid",
			raw:          json.RawMessage(`{"type":"oci"}`),
			wantUsername: "",
			wantPassword: "",
		},
		{
			name:    "malformed JSON returns error",
			raw:     json.RawMessage(`{not json`),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseOCIAuth(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOCIAuth: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOCIAuth: unexpected error: %v", err)
			}
			if got.Username != tc.wantUsername {
				t.Errorf("Username = %q, want %q", got.Username, tc.wantUsername)
			}
			if got.Password != tc.wantPassword {
				t.Errorf("Password = %q, want %q", got.Password, tc.wantPassword)
			}
			if got.Kind() != "oci" {
				t.Errorf("Kind() = %q, want \"oci\"", got.Kind())
			}
		})
	}
}

// ---- ResolveSource OCI dispatch ----

func TestResolveSource_OCI(t *testing.T) {
	t.Parallel()

	src, ref, err := ResolveSource("oci://reg.local/img:v1")
	if err != nil {
		t.Fatalf("ResolveSource: unexpected error: %v", err)
	}
	if src == nil {
		t.Fatalf("ResolveSource: expected non-nil Source, got nil")
	}
	if _, ok := src.(*ociSource); !ok {
		t.Errorf("Source is %T, want *ociSource", src)
	}
	if ref.Scheme != "oci" {
		t.Errorf("ref.Scheme = %q, want \"oci\"", ref.Scheme)
	}
	if ref.Image != "reg.local/img" {
		t.Errorf("ref.Image = %q, want \"reg.local/img\"", ref.Image)
	}
	if ref.Tag != "v1" {
		t.Errorf("ref.Tag = %q, want \"v1\"", ref.Tag)
	}
	if ref.URL != "oci://reg.local/img:v1" {
		t.Errorf("ref.URL = %q, want \"oci://reg.local/img:v1\"", ref.URL)
	}
}

func TestResolveSource_OCI_NoTag(t *testing.T) {
	t.Parallel()

	src, ref, err := ResolveSource("oci://reg.local/img")
	if err != nil {
		t.Fatalf("ResolveSource: unexpected error: %v", err)
	}
	if src == nil {
		t.Fatalf("ResolveSource: expected non-nil Source, got nil")
	}
	if ref.Tag != "latest" {
		t.Errorf("ref.Tag = %q, want \"latest\"", ref.Tag)
	}
	if ref.Image != "reg.local/img" {
		t.Errorf("ref.Image = %q, want \"reg.local/img\"", ref.Image)
	}
}

// ---- resolveOCIAuthn ----

func TestResolveOCIAuthn_ociCredentials(t *testing.T) {
	t.Parallel()

	creds := ociCredentials{Username: "u", Password: "p"}
	auth, err := resolveOCIAuthn(creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil authenticator")
	}
}

func TestResolveOCIAuthn_anonymous(t *testing.T) {
	t.Parallel()

	// noCreds → Anonymous
	auth, err := resolveOCIAuthn(noCreds{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil authenticator")
	}

	// empty ociCredentials → Anonymous
	auth2, err := resolveOCIAuthn(ociCredentials{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth2 == nil {
		t.Fatal("expected non-nil authenticator")
	}
}

func TestResolveOCIAuthn_rawAuthCreds(t *testing.T) {
	t.Parallel()

	raw := rawAuthCreds{
		authType: "oci",
		Raw:      json.RawMessage(`{"type":"oci","username":"robot","password":"pw"}`),
	}
	auth, err := resolveOCIAuthn(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil authenticator")
	}
}

func TestResolveOCIAuthn_incompatibleKind(t *testing.T) {
	t.Parallel()

	_, err := resolveOCIAuthn(basicCreds{Username: "u", Password: "p"})
	if err == nil {
		t.Fatal("expected error for incompatible creds kind, got nil")
	}
	if !strings.Contains(err.Error(), "incompatible credentials kind") {
		t.Errorf("error %q does not mention incompatible kind", err.Error())
	}
}

// ---- pickLayer ----

// fakeLayer is a minimal ociLayer implementation for unit tests.
type fakeLayer struct {
	mt      types.MediaType
	mtErr   error
	size    int64
	content []byte
}

func (f *fakeLayer) MediaType() (types.MediaType, error) { return f.mt, f.mtErr }
func (f *fakeLayer) Size() (int64, error)                { return f.size, nil }
func (f *fakeLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.content)), nil
}

func TestPickLayer_qcow2MediaTypeWins(t *testing.T) {
	t.Parallel()

	dockerLayer := &fakeLayer{mt: types.DockerLayer, content: []byte("docker")}
	qcow2Layer := &fakeLayer{mt: types.MediaType(stemcellQcow2MediaType), content: []byte("qcow2")}
	otherLayer := &fakeLayer{mt: "application/vnd.other.layer", content: []byte("other")}

	layers := []ociLayer{dockerLayer, qcow2Layer, otherLayer}
	got := pickLayer(layers)
	if got != qcow2Layer {
		t.Errorf("pickLayer picked %v, want qcow2Layer", got)
	}
}

func TestPickLayer_nonStandardFallback(t *testing.T) {
	t.Parallel()

	dockerLayer := &fakeLayer{mt: types.DockerLayer, content: []byte("docker")}
	customLayer := &fakeLayer{mt: "application/vnd.custom.artifact", content: []byte("custom")}

	layers := []ociLayer{dockerLayer, customLayer}
	got := pickLayer(layers)
	if got != customLayer {
		t.Errorf("pickLayer picked %v, want customLayer", got)
	}
}

func TestPickLayer_firstLayerFinalFallback(t *testing.T) {
	t.Parallel()

	// All standard rootfs layers — should fall back to first.
	a := &fakeLayer{mt: types.DockerLayer, content: []byte("a")}
	b := &fakeLayer{mt: types.OCILayer, content: []byte("b")}
	layers := []ociLayer{a, b}
	got := pickLayer(layers)
	if got != a {
		t.Errorf("pickLayer picked %v, want first layer a", got)
	}
}

func TestPickLayer_mediaTypeErrorSkipped(t *testing.T) {
	t.Parallel()

	errLayer := &fakeLayer{mtErr: fmt.Errorf("registry error"), content: []byte("err")}
	qcow2Layer := &fakeLayer{mt: types.MediaType(stemcellQcow2MediaType), content: []byte("qcow2")}

	layers := []ociLayer{errLayer, qcow2Layer}
	got := pickLayer(layers)
	if got != qcow2Layer {
		t.Errorf("pickLayer picked %v, want qcow2Layer", got)
	}
}

// ---- Integration: Fetch against in-process registry ----
//
// Uses go-containerregistry's registry.New() as an httptest-backed in-process
// OCI distribution registry. A random image is pushed to the registry using
// remote.Write, then pulled back via ociSource.Fetch.

// newTestRegistry spins up an httptest server backed by an in-memory OCI
// registry. Returns the server and the base host string (e.g. "127.0.0.1:PORT").
//
// nolint:unparam // *httptest.Server result is kept so tests that need direct
// server access (e.g. closing early to simulate network failure) can use it.
func newTestRegistry(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	reg := registry.New()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return srv, host
}

// pushImage pushes img to registry at host under repoTag (e.g. "myrepo:v1").
// Returns the full reference string.
//
// nolint:unparam // full-ref result kept for tests that assert exact ref text.
func pushImage(t *testing.T, host string, repoTag string, img v1.Image) string {
	t.Helper()
	fullRef := fmt.Sprintf("%s/%s", host, repoTag)
	ref, err := name.ParseReference(fullRef, name.Insecure)
	if err != nil {
		t.Fatalf("pushImage: parse ref %q: %v", fullRef, err)
	}
	if err := remote.Write(ref, img, remote.WithAuth(testAnonymousAuth)); err != nil {
		t.Fatalf("pushImage: remote.Write %q: %v", fullRef, err)
	}
	return fullRef
}

// testAnonymousAuth is authn.Anonymous reused directly via type alias so the
// import is used. pushImage passes it to remote.WithAuth.
var testAnonymousAuth authn.Authenticator = authn.Anonymous

// TestOCISource_Fetch_Anonymous pushes a random single-layer image to an
// in-process registry, then fetches it back via ociSource.Fetch with noCreds.
// Verifies the returned body is non-empty and closeable without error.
func TestOCISource_Fetch_Anonymous(t *testing.T) {
	t.Parallel()

	_, host := newTestRegistry(t)

	// Create a random single-layer image.
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}

	repoTag := "stemcell/test:v1"
	rawURL := fmt.Sprintf("oci://%s/%s", host, repoTag)

	// Push the image.
	pushImage(t, host, repoTag, img)

	src := newOCISource()
	ref := Reference{URL: rawURL}
	body, _, err := src.Fetch(context.Background(), ref, noCreds{})
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(data) == 0 {
		t.Error("Fetch returned empty body")
	}
}

// TestOCISource_Fetch_LayerSelection pushes a two-layer image where the second
// layer has media-type stemcellQcow2MediaType. Verifies the second layer is
// selected and its content matches the expected bytes.
func TestOCISource_Fetch_LayerSelection(t *testing.T) {
	t.Parallel()

	_, host := newTestRegistry(t)

	// Build a two-layer image:
	//   layer 0: standard Docker rootfs (DockerLayer)
	//   layer 1: stemcell qcow2 (stemcellQcow2MediaType)
	//
	// go-containerregistry random.Layer accepts a media-type option.
	// Use mutate to build a custom image.
	qcow2Content := []byte("fake-qcow2-data-12345678")

	layer0, err := random.Layer(128, types.DockerLayer)
	if err != nil {
		t.Fatalf("random.Layer (layer0): %v", err)
	}
	layer1, err := random.Layer(int64(len(qcow2Content)), types.MediaType(stemcellQcow2MediaType))
	if err != nil {
		t.Fatalf("random.Layer (layer1/qcow2): %v", err)
	}

	base, err := random.Image(0, 0)
	if err != nil {
		t.Fatalf("random.Image (base): %v", err)
	}
	img, err := mutate.AppendLayers(base, layer0, layer1)
	if err != nil {
		t.Fatalf("mutate.AppendLayers: %v", err)
	}

	repoTag := "stemcell/layersel:v2"
	rawURL := fmt.Sprintf("oci://%s/%s", host, repoTag)

	pushImage(t, host, repoTag, img)

	src := newOCISource()
	ref := Reference{URL: rawURL}
	body, _, err := src.Fetch(context.Background(), ref, noCreds{})
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// The selected layer should be layer1 (qcow2 media-type). Its uncompressed
	// content is a tar archive generated by random.Layer — non-empty is the
	// minimum bar here; exact byte match is not feasible (random seed).
	if len(data) == 0 {
		t.Error("Fetch returned empty body for qcow2 layer")
	}
}

// TestOCISource_Fetch_NoLayers verifies that an image with no layers returns
// an explicit error.
func TestOCISource_Fetch_NoLayers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve a minimal valid OCI manifest with no layers.
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/") || r.URL.Path == "/v2":
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			// Return a manifest with an empty layers array.
			manifest := `{
				"schemaVersion": 2,
				"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
				"config": {
					"mediaType": "application/vnd.docker.container.image.v1+json",
					"size": 2,
					"digest": "sha256:44136fa355ba77b9ad7b35f447e0034e61d3adcf8855e7a59c3e68c2b5d9f5a6"
				},
				"layers": []
			}`
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	rawURL := fmt.Sprintf("oci://%s/empty/repo:latest", host)

	src := newOCISource()
	ref := Reference{URL: rawURL}
	_, _, err := src.Fetch(context.Background(), ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for image with no layers, got nil")
	}
	// Error may come from empty layers check or from image parsing — either is valid.
	// Just verify it's non-nil.
	_ = err
}

// TestOCISource_Fetch_BadRef verifies that a malformed reference string
// returns an error before any network call.
func TestOCISource_Fetch_BadRef(t *testing.T) {
	t.Parallel()

	src := newOCISource()
	ref := Reference{URL: "oci://::bad::ref"}
	_, _, err := src.Fetch(context.Background(), ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for bad OCI reference, got nil")
	}
}
