package stemcellfetch

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---- ResolveSource ----

func TestResolveSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rawURL     string
		wantScheme string
		wantBlobID string
		// wantSuccess: true → expect nil error and non-nil Source.
		// false → expect a non-nil error (errNotImplemented, unsupported, or empty).
		wantSuccess     bool
		wantErrContains string
		wantUnsupported bool // true → expect "unsupported URL scheme" error
		wantEmptyURLErr bool // true → expect empty-URL error specifically
	}{
		{
			name:            "empty URL returns error",
			rawURL:          "",
			wantEmptyURLErr: true,
		},
		{
			name:        "https scheme resolves to httpsSource with no error",
			rawURL:      "https://example.com/stemcell.tgz",
			wantScheme:  "https",
			wantSuccess: true,
		},
		{
			name:        "s3 scheme resolves to s3Source with populated Reference",
			rawURL:      "s3://my-bucket/stemcells/ubuntu.qcow2",
			wantScheme:  "s3",
			wantSuccess: true,
		},
		{
			name:        "bosh+blobstore scheme resolves to blobstoreSource with populated Reference",
			rawURL:      "bosh+blobstore:abc-123-def-456",
			wantScheme:  "bosh+blobstore",
			wantBlobID:  "abc-123-def-456",
			wantSuccess: true,
		},
		{
			name:        "oci scheme resolves to ociSource with populated Reference",
			rawURL:      "oci://registry.example.com/repo/stemcell:latest",
			wantScheme:  "oci",
			wantSuccess: true,
		},
		{
			name:            "unknown scheme returns unsupported error",
			rawURL:          "ftp://example.com/stemcell.tgz",
			wantUnsupported: true,
		},
		{
			name:            "bare filename with no scheme is unsupported",
			rawURL:          "stemcell.qcow2",
			wantUnsupported: true,
		},
		{
			name:            "http (not https) is unsupported",
			rawURL:          "http://example.com/stemcell.tgz",
			wantUnsupported: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src, ref, err := ResolveSource(tc.rawURL)

			// Success path: wired schemes return nil error and non-nil Source.
			if tc.wantSuccess {
				if err != nil {
					t.Fatalf("ResolveSource(%q): unexpected error: %v", tc.rawURL, err)
				}
				if src == nil {
					t.Fatalf("ResolveSource(%q): expected non-nil Source, got nil", tc.rawURL)
				}
				if ref.Scheme != tc.wantScheme {
					t.Errorf("ref.Scheme = %q, want %q", ref.Scheme, tc.wantScheme)
				}
				if ref.URL != tc.rawURL {
					t.Errorf("ref.URL = %q, want %q", ref.URL, tc.rawURL)
				}
				if tc.wantBlobID != "" && ref.BlobID != tc.wantBlobID {
					t.Errorf("ref.BlobID = %q, want %q", ref.BlobID, tc.wantBlobID)
				}
				return
			}

			// All remaining paths return a non-nil error: either empty-URL,
			// unsupported scheme, or errNotImplemented for known schemes.
			if err == nil {
				t.Fatalf("ResolveSource(%q): expected error, got nil (src=%v)", tc.rawURL, src)
			}

			if tc.wantEmptyURLErr {
				if !strings.Contains(err.Error(), "image_url is empty") {
					t.Errorf("expected empty-URL error, got: %v", err)
				}
				return
			}

			if tc.wantUnsupported {
				if !strings.Contains(err.Error(), "unsupported URL scheme") {
					t.Errorf("expected unsupported-scheme error, got: %v", err)
				}
				// Unsupported scheme: ref.Scheme is empty (no known scheme set).
				// ref.URL should still carry the original URL.
				if ref.URL != tc.rawURL {
					t.Errorf("ref.URL = %q, want %q", ref.URL, tc.rawURL)
				}
				return
			}

			// Known-but-not-implemented schemes: check errNotImplemented message.
			if !strings.Contains(err.Error(), "not yet implemented") {
				t.Errorf("expected errNotImplemented, got: %v", err)
			}

			if ref.Scheme != tc.wantScheme {
				t.Errorf("ref.Scheme = %q, want %q", ref.Scheme, tc.wantScheme)
			}
			if ref.URL != tc.rawURL {
				t.Errorf("ref.URL = %q, want %q", ref.URL, tc.rawURL)
			}
			if tc.wantBlobID != "" && ref.BlobID != tc.wantBlobID {
				t.Errorf("ref.BlobID = %q, want %q", ref.BlobID, tc.wantBlobID)
			}
			// Source impl is nil for not-yet-wired schemes.
			if src != nil {
				t.Errorf("expected nil Source for not-yet-wired scheme, got %T", src)
			}
		})
	}
}

// ---- ResolveCredentials ----

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestResolveCredentials(t *testing.T) {
	t.Parallel()

	basicDefault := config.FetchCredentialDefault{
		URLPrefix: "https://corp.example.com/",
		Auth:      mustRaw(map[string]string{"type": "basic", "username": "u", "password": "p"}),
	}
	bearerSub := config.FetchCredentialDefault{
		URLPrefix: "https://corp.example.com/sub/",
		Auth:      mustRaw(map[string]string{"type": "bearer", "bearer_token": "long-match-token"}),
	}

	tests := []struct {
		name      string
		propsAuth json.RawMessage
		defaults  []config.FetchCredentialDefault
		url       string
		wantKind  string
		wantErr   bool
	}{
		{
			name:      "per-stemcell bearer overrides defaults",
			propsAuth: mustRaw(map[string]string{"type": "bearer", "bearer_token": "props-token"}),
			defaults:  []config.FetchCredentialDefault{basicDefault},
			url:       "https://corp.example.com/stemcell.tgz",
			wantKind:  "bearer",
		},
		{
			name:      "per-stemcell basic auth, no defaults",
			propsAuth: mustRaw(map[string]string{"type": "basic", "username": "u", "password": "p"}),
			defaults:  nil,
			url:       "https://anything.example.com/",
			wantKind:  "basic",
		},
		{
			name:      "defaults basic match on URL prefix",
			propsAuth: nil,
			defaults:  []config.FetchCredentialDefault{basicDefault},
			url:       "https://corp.example.com/path/stemcell.tgz",
			wantKind:  "basic",
		},
		{
			name:      "longest-prefix wins over shorter match",
			propsAuth: nil,
			defaults:  []config.FetchCredentialDefault{basicDefault, bearerSub},
			url:       "https://corp.example.com/sub/foo/stemcell.tgz",
			wantKind:  "bearer",
		},
		{
			name:      "no match returns noCreds",
			propsAuth: nil,
			defaults:  []config.FetchCredentialDefault{basicDefault},
			url:       "https://other.example.com/stemcell.tgz",
			wantKind:  "none",
		},
		{
			name:      "empty defaults and no propsAuth returns noCreds",
			propsAuth: nil,
			defaults:  nil,
			url:       "https://anything.example.com/",
			wantKind:  "none",
		},
		{
			name:      "null propsAuth treated as absent",
			propsAuth: json.RawMessage("null"),
			defaults:  nil,
			url:       "https://anything.example.com/",
			wantKind:  "none",
		},
		{
			name:      "unknown auth type returns error",
			propsAuth: mustRaw(map[string]string{"type": "unknown-scheme"}),
			defaults:  nil,
			url:       "https://anything.example.com/",
			wantErr:   true,
		},
		{
			name:      "auth payload with missing type field returns error",
			propsAuth: json.RawMessage(`{}`),
			defaults:  nil,
			url:       "https://anything.example.com/",
			wantErr:   true,
		},
		{
			name:      "s3 auth type with valid credentials returns s3Credentials",
			propsAuth: mustRaw(map[string]interface{}{"type": "s3", "access_key_id": "AKIATEST", "secret_access_key": "secretval"}),
			defaults:  nil,
			url:       "s3://bucket/key",
			wantKind:  "s3",
		},
		{
			name:      "s3 auth type missing credentials returns error",
			propsAuth: mustRaw(map[string]string{"type": "s3"}),
			defaults:  nil,
			url:       "s3://bucket/key",
			wantErr:   true,
		},
		{
			name:      "blobstore auth type with valid endpoint returns blobstoreCredentials",
			propsAuth: mustRaw(map[string]string{"type": "blobstore", "endpoint": "https://blobstore.example.com"}),
			defaults:  nil,
			url:       "bosh+blobstore:abc",
			wantKind:  "blobstore",
		},
		{
			name:      "blobstore auth type missing endpoint returns error",
			propsAuth: mustRaw(map[string]string{"type": "blobstore"}),
			defaults:  nil,
			url:       "bosh+blobstore:abc",
			wantErr:   true,
		},
		{
			name:      "oci auth type returns ociCredentials",
			propsAuth: mustRaw(map[string]string{"type": "oci"}),
			defaults:  nil,
			url:       "oci://reg/repo:tag",
			wantKind:  "oci",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			creds, err := ResolveCredentials(tc.propsAuth, tc.defaults, tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (creds=%v)", creds)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if creds.Kind() != tc.wantKind {
				t.Errorf("Kind() = %q, want %q", creds.Kind(), tc.wantKind)
			}
		})
	}
}

// ---- basicCreds.Apply ----

func TestBasicCreds_Apply(t *testing.T) {
	t.Parallel()

	bc := basicCreds{Username: "alice", Password: "s3cr3t"}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if err := bc.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	authHeader := req.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Basic ") {
		t.Fatalf("Authorization header = %q, want prefix \"Basic \"", authHeader)
	}

	// Verify the encoded credentials decode back to the original user/pass.
	gotUser, gotPass, ok := req.BasicAuth()
	if !ok {
		t.Fatalf("req.BasicAuth() returned ok=false; header = %q", authHeader)
	}
	if gotUser != "alice" {
		t.Errorf("decoded username = %q, want \"alice\"", gotUser)
	}
	if gotPass != "s3cr3t" {
		t.Errorf("decoded password = %q, want \"s3cr3t\"", gotPass)
	}

	if bc.Kind() != "basic" {
		t.Errorf("Kind() = %q, want \"basic\"", bc.Kind())
	}
}

// ---- bearerCreds.Apply ----

func TestBearerCreds_Apply(t *testing.T) {
	t.Parallel()

	token := "my-bearer-token-12345"
	br := bearerCreds{BearerToken: token}

	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if err := br.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := "Bearer " + token
	got := req.Header.Get("Authorization")
	if got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}

	if br.Kind() != "bearer" {
		t.Errorf("Kind() = %q, want \"bearer\"", br.Kind())
	}
}

// ---- noCreds ----

func TestNoCreds(t *testing.T) {
	t.Parallel()

	var nc noCreds

	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if err := nc.Apply(req); err != nil {
		t.Errorf("noCreds.Apply: unexpected error: %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("noCreds.Apply set Authorization header, expected none")
	}
	if nc.Kind() != "none" {
		t.Errorf("Kind() = %q, want \"none\"", nc.Kind())
	}
}

// ---- longestPrefixMatch edge cases ----

func TestLongestPrefixMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		defaults []config.FetchCredentialDefault
		url      string
		wantNil  bool
		wantPfx  string
	}{
		{
			name:     "nil defaults returns nil",
			defaults: nil,
			url:      "https://example.com/x",
			wantNil:  true,
		},
		{
			name:     "empty defaults returns nil",
			defaults: []config.FetchCredentialDefault{},
			url:      "https://example.com/x",
			wantNil:  true,
		},
		{
			name: "entry with empty URLPrefix is skipped",
			defaults: []config.FetchCredentialDefault{
				{URLPrefix: "", Auth: mustRaw(map[string]string{"type": "basic", "username": "x", "password": "y"})},
			},
			url:     "https://anything.com/x",
			wantNil: true,
		},
		{
			name: "single match returned",
			defaults: []config.FetchCredentialDefault{
				{URLPrefix: "https://a.com/", Auth: mustRaw(map[string]string{"type": "basic", "username": "x", "password": "y"})},
			},
			url:     "https://a.com/foo",
			wantPfx: "https://a.com/",
		},
		{
			name: "no match returns nil",
			defaults: []config.FetchCredentialDefault{
				{URLPrefix: "https://a.com/", Auth: mustRaw(map[string]string{"type": "basic", "username": "x", "password": "y"})},
			},
			url:     "https://b.com/foo",
			wantNil: true,
		},
		{
			name: "longest prefix among multiple matches wins",
			defaults: []config.FetchCredentialDefault{
				{URLPrefix: "https://a.com/", Auth: mustRaw(map[string]string{"type": "basic", "username": "x", "password": "y"})},
				{URLPrefix: "https://a.com/sub/", Auth: mustRaw(map[string]string{"type": "bearer", "bearer_token": "t"})},
				{URLPrefix: "https://a.com/sub/deep/", Auth: mustRaw(map[string]string{"type": "bearer", "bearer_token": "t2"})},
			},
			url:     "https://a.com/sub/deep/stemcell.tgz",
			wantPfx: "https://a.com/sub/deep/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := longestPrefixMatch(tc.defaults, tc.url)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got entry with prefix %q", got.URLPrefix)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil result with prefix %q, got nil", tc.wantPfx)
			}
			if got.URLPrefix != tc.wantPfx {
				t.Errorf("URLPrefix = %q, want %q", got.URLPrefix, tc.wantPfx)
			}
		})
	}
}
