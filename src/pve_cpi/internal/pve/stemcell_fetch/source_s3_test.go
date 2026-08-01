package stemcellfetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- parseS3URL ----

func TestParseS3URL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rawURL     string
		wantBucket string
		wantKey    string
		wantErr    string // substring expected in error; empty → no error
	}{
		{
			name:       "valid single-segment key",
			rawURL:     "s3://my-bucket/stemcell.qcow2",
			wantBucket: "my-bucket",
			wantKey:    "stemcell.qcow2",
		},
		{
			name:       "valid multi-segment key",
			rawURL:     "s3://my-bucket/stemcells/ubuntu/jammy/stemcell.qcow2",
			wantBucket: "my-bucket",
			wantKey:    "stemcells/ubuntu/jammy/stemcell.qcow2",
		},
		{
			name:    "missing s3:// prefix",
			rawURL:  "https://my-bucket/key",
			wantErr: "missing s3:// prefix",
		},
		{
			name:    "bucket only no key",
			rawURL:  "s3://my-bucket",
			wantErr: "missing key after bucket",
		},
		{
			name:    "empty bucket",
			rawURL:  "s3:///key",
			wantErr: "has empty bucket or key",
		},
		{
			name:    "empty key",
			rawURL:  "s3://my-bucket/",
			wantErr: "has empty bucket or key",
		},
		{
			name:    "empty string",
			rawURL:  "",
			wantErr: "missing s3:// prefix",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bucket, key, err := parseS3URL(tc.rawURL)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseS3URL(%q): expected error containing %q, got nil", tc.rawURL, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("parseS3URL(%q): error = %q, want substring %q", tc.rawURL, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseS3URL(%q): unexpected error: %v", tc.rawURL, err)
			}
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, tc.wantBucket)
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
		})
	}
}

// ---- parseS3Auth ----

func TestParseS3Auth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		payload         map[string]any
		wantAccessKeyID string
		wantRegion      string
		wantEndpoint    string
		wantPathStyle   bool
		wantErr         string
	}{
		{
			name: "valid minimal credentials",
			payload: map[string]any{
				"type":              "s3",
				"access_key_id":     "AKIATEST123",
				"secret_access_key": "secretvalue",
			},
			wantAccessKeyID: "AKIATEST123",
		},
		{
			name: "valid full credentials with endpoint and region",
			payload: map[string]any{
				"type":              "s3",
				"access_key_id":     "AKIATEST456",
				"secret_access_key": "secretvalue",
				"endpoint":          "https://minio.lab.local",
				"region":            "eu-west-1",
				"force_path_style":  true,
			},
			wantAccessKeyID: "AKIATEST456",
			wantRegion:      "eu-west-1",
			wantEndpoint:    "https://minio.lab.local",
			wantPathStyle:   true,
		},
		{
			name: "missing access_key_id",
			payload: map[string]any{
				"type":              "s3",
				"secret_access_key": "secretvalue",
			},
			wantErr: "access_key_id and secret_access_key are required",
		},
		{
			name: "missing secret_access_key",
			payload: map[string]any{
				"type":          "s3",
				"access_key_id": "AKIATEST",
			},
			wantErr: "access_key_id and secret_access_key are required",
		},
		{
			name:    "both keys missing",
			payload: map[string]any{"type": "s3"},
			wantErr: "access_key_id and secret_access_key are required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}

			c, err := parseS3Auth(raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseS3Auth: expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseS3Auth: unexpected error: %v", err)
			}
			if c.AccessKeyID != tc.wantAccessKeyID {
				t.Errorf("AccessKeyID = %q, want %q", c.AccessKeyID, tc.wantAccessKeyID)
			}
			if tc.wantRegion != "" && c.Region != tc.wantRegion {
				t.Errorf("Region = %q, want %q", c.Region, tc.wantRegion)
			}
			if tc.wantEndpoint != "" && c.Endpoint != tc.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", c.Endpoint, tc.wantEndpoint)
			}
			if tc.wantPathStyle && !c.ForcePathStyle {
				t.Errorf("ForcePathStyle = false, want true")
			}
			if c.Kind() != "s3" {
				t.Errorf("Kind() = %q, want \"s3\"", c.Kind())
			}
		})
	}
}

// s3TestServer returns an httptest.Server that handles path-style S3 requests.
// The returned cleanup func closes the server.
//
// Handled paths:
//   - GET /test-bucket/test-key → 200 + "stemcell-data" body
//   - GET /test-bucket/missing-key → 404
//   - All others → 404
//
// The server does not validate auth headers — auth correctness is handled by
// the AWS SDK's signing layer. The purpose is to verify the S3 source wires
// the endpoint + bucket/key correctly, not to replicate full AWS auth.
func s3TestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-bucket/test-key":
			w.Header().Set("Content-Length", "13")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("stemcell-data"))
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

// TestS3Source_Fetch_Success: valid endpoint override (httptest URL) + creds;
// bucket/key returns body with correct content.
func TestS3Source_Fetch_Success(t *testing.T) {
	t.Parallel()

	server := s3TestServer(t)
	defer server.Close()

	ctx := context.Background()
	ref := Reference{URL: "s3://test-bucket/test-key", Scheme: "s3", Bucket: "test-bucket", Key: "test-key"}
	creds := s3Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secretvalue",
		Endpoint:        server.URL,
	}
	src := newS3Source()

	body, size, err := src.Fetch(ctx, ref, creds)
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "stemcell-data" {
		t.Errorf("body = %q, want \"stemcell-data\"", got)
	}
	if size != 13 {
		t.Errorf("contentLength = %d, want 13", size)
	}
}

// TestS3Source_Fetch_ViaRawAuthCreds: credentials arrive as rawAuthCreds
// via the generic parseAuth path; source decodes them inline.
func TestS3Source_Fetch_ViaRawAuthCreds(t *testing.T) {
	t.Parallel()

	server := s3TestServer(t)
	defer server.Close()

	raw, err := json.Marshal(map[string]any{
		"type":              "s3",
		"access_key_id":     "AKIATEST",
		"secret_access_key": "secretvalue",
		"endpoint":          server.URL,
	})
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}

	ctx := context.Background()
	ref := Reference{URL: "s3://test-bucket/test-key", Scheme: "s3", Bucket: "test-bucket", Key: "test-key"}
	src := newS3Source()

	body, _, err := src.Fetch(ctx, ref, rawAuthCreds{authType: "s3", Raw: raw})
	if err != nil {
		t.Fatalf("Fetch via rawAuthCreds: unexpected error: %v", err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "stemcell-data" {
		t.Errorf("body = %q, want \"stemcell-data\"", got)
	}
}

// TestS3Source_Fetch_MissingKey: server returns 404; error surfaced.
func TestS3Source_Fetch_MissingKey(t *testing.T) {
	t.Parallel()

	server := s3TestServer(t)
	defer server.Close()

	ctx := context.Background()
	ref := Reference{URL: "s3://test-bucket/missing-key", Scheme: "s3", Bucket: "test-bucket", Key: "missing-key"}
	creds := s3Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "secretvalue",
		Endpoint:        server.URL,
	}
	src := newS3Source()

	_, _, err := src.Fetch(ctx, ref, creds)
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
	// AWS SDK wraps 404 from S3 as a NoSuchKey error; message includes bucket/key context.
	if !strings.Contains(err.Error(), "test-bucket") && !strings.Contains(err.Error(), "missing-key") &&
		!strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("error missing key/bucket context: %v", err)
	}
}

// TestS3Source_Fetch_NoCreds: passing noCreds → error mentions "credentials required".
func TestS3Source_Fetch_NoCreds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ref := Reference{URL: "s3://test-bucket/test-key", Scheme: "s3", Bucket: "test-bucket", Key: "test-key"}
	src := newS3Source()

	_, _, err := src.Fetch(ctx, ref, noCreds{})
	if err == nil {
		t.Fatal("expected error for noCreds, got nil")
	}
	if !strings.Contains(err.Error(), "credentials required") {
		t.Errorf("error missing 'credentials required': %v", err)
	}
}

// TestS3Source_Fetch_IncompatibleCreds: incompatible creds type → error.
func TestS3Source_Fetch_IncompatibleCreds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ref := Reference{URL: "s3://test-bucket/test-key", Scheme: "s3", Bucket: "test-bucket", Key: "test-key"}
	src := newS3Source()

	_, _, err := src.Fetch(ctx, ref, basicCreds{Username: "u", Password: "p"})
	if err == nil {
		t.Fatal("expected error for incompatible creds, got nil")
	}
	if !strings.Contains(err.Error(), "incompatible credentials kind") {
		t.Errorf("error missing 'incompatible credentials kind': %v", err)
	}
}

// TestResolveSource_S3: s3://bucket/key → non-nil source + populated Reference.
func TestResolveSource_S3(t *testing.T) {
	t.Parallel()

	rawURL := "s3://prod-stemcells/ubuntu/jammy/stemcell.qcow2"
	src, ref, err := ResolveSourceWith(rawURL, DefaultTransportConfig())
	if err != nil {
		t.Fatalf("ResolveSourceWith(%q, DefaultTransportConfig()): unexpected error: %v", rawURL, err)
	}
	if src == nil {
		t.Fatalf("ResolveSourceWith(%q, DefaultTransportConfig()): expected non-nil Source, got nil", rawURL)
	}
	if ref.Scheme != "s3" {
		t.Errorf("ref.Scheme = %q, want \"s3\"", ref.Scheme)
	}
	if ref.Bucket != "prod-stemcells" {
		t.Errorf("ref.Bucket = %q, want \"prod-stemcells\"", ref.Bucket)
	}
	if ref.Key != "ubuntu/jammy/stemcell.qcow2" {
		t.Errorf("ref.Key = %q, want \"ubuntu/jammy/stemcell.qcow2\"", ref.Key)
	}
	if ref.URL != rawURL {
		t.Errorf("ref.URL = %q, want %q", ref.URL, rawURL)
	}
}

// TestResolveSource_S3_InvalidURL: malformed s3:// URLs return an error (not a source).
func TestResolveSource_S3_InvalidURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rawURL string
	}{
		{"bucket only", "s3://only-bucket"},
		{"empty key", "s3://bucket/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src, _, err := ResolveSourceWith(tc.rawURL, DefaultTransportConfig())
			if err == nil {
				t.Fatalf("ResolveSourceWith(%q, DefaultTransportConfig()): expected error, got nil (src=%T)", tc.rawURL, src)
			}
			if src != nil {
				t.Errorf("ResolveSourceWith(%q, DefaultTransportConfig()): expected nil Source on error, got %T", tc.rawURL, src)
			}
		})
	}
}

// TestParseAuth_S3: RawMessage with type=s3 and valid keys → returns s3Credentials (not rawAuthCreds).
func TestParseAuth_S3(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(map[string]any{
		"type":              "s3",
		"access_key_id":     "AKIATEST789",
		"secret_access_key": "supersecret",
		"endpoint":          "https://r2.cloudflare.com",
		"region":            "auto",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	creds, err := parseAuth(raw)
	if err != nil {
		t.Fatalf("parseAuth: unexpected error: %v", err)
	}

	c, ok := creds.(s3Credentials)
	if !ok {
		t.Fatalf("parseAuth returned %T, want s3Credentials", creds)
	}
	if c.AccessKeyID != "AKIATEST789" {
		t.Errorf("AccessKeyID = %q, want \"AKIATEST789\"", c.AccessKeyID)
	}
	if c.Endpoint != "https://r2.cloudflare.com" {
		t.Errorf("Endpoint = %q, want \"https://r2.cloudflare.com\"", c.Endpoint)
	}
	if c.Region != "auto" {
		t.Errorf("Region = %q, want \"auto\"", c.Region)
	}
	if creds.Kind() != "s3" {
		t.Errorf("Kind() = %q, want \"s3\"", creds.Kind())
	}
}

// TestS3Credentials_Apply: Apply is a no-op (S3 uses SDK SigV4, not headers).
func TestS3Credentials_Apply(t *testing.T) {
	t.Parallel()

	c := s3Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := c.Apply(req); err != nil {
		t.Fatalf("Apply: unexpected error: %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("Apply set Authorization header; expected no-op")
	}
	if c.Kind() != "s3" {
		t.Errorf("Kind() = %q, want \"s3\"", c.Kind())
	}
}
