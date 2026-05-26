package stemcellfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Source implements Source for s3:// references. Uses aws-sdk-go-v2 with
// optional endpoint override for S3-compatible backends (MinIO, R2).
//
// Failure modes handled by Fetch:
//   - non-s3:// URL → error from parseS3URL
//   - wrong/incompatible Credentials type → error
//   - noCreds → explicit "credentials required" error
//   - missing access_key_id or secret_access_key → error from parseS3Auth
//   - AWS SDK config load failure → wrapped error
//   - GetObject network/credential error → wrapped error from SDK
//   - key not found (404/NoSuchKey) → wrapped error from SDK
type s3Source struct{}

func newS3Source() *s3Source { return &s3Source{} }

// s3Credentials is the concrete Credentials type for type="s3" auth payloads.
// Apply is a no-op: the S3 source uses AWS SigV4 request signing via the SDK,
// not HTTP header injection.
//
// JSON fields match the credential payload schema:
//
//	{"type":"s3","access_key_id":"...","secret_access_key":"...","endpoint":"...","region":"...","force_path_style":true}
//
// endpoint, region, and force_path_style are optional.
// Default region is "us-east-1" when absent.
// When endpoint is set, path-style addressing is enabled automatically (MinIO/R2 mode).
type s3Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Endpoint        string `json:"endpoint,omitempty"`
	Region          string `json:"region,omitempty"`
	ForcePathStyle  bool   `json:"force_path_style,omitempty"`
}

func (s3Credentials) Apply(_ *http.Request) error { return nil }
func (s3Credentials) Kind() string                { return "s3" }

// parseS3Auth deserializes raw into s3Credentials.
//
// Failure modes:
//   - malformed JSON → wrapped unmarshal error
//   - missing access_key_id or secret_access_key → validation error
func parseS3Auth(raw json.RawMessage) (s3Credentials, error) {
	var c s3Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("stemcell_fetch(s3): parse credentials: %w", err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return c, fmt.Errorf("stemcell_fetch(s3): access_key_id and secret_access_key are required")
	}
	return c, nil
}

// parseS3URL extracts bucket and key from an s3://bucket/key URL.
//
// Failure modes:
//   - missing s3:// prefix → error
//   - no slash after bucket (key absent) → error
//   - empty bucket or empty key → error
func parseS3URL(rawURL string) (bucket, key string, err error) {
	if !strings.HasPrefix(rawURL, "s3://") {
		return "", "", fmt.Errorf("stemcell_fetch(s3): URL %q missing s3:// prefix", rawURL)
	}
	rest := strings.TrimPrefix(rawURL, "s3://")
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return "", "", fmt.Errorf("stemcell_fetch(s3): URL %q missing key after bucket", rawURL)
	}
	bucket = rest[:idx]
	key = rest[idx+1:]
	if bucket == "" || key == "" {
		return "", "", fmt.Errorf("stemcell_fetch(s3): URL %q has empty bucket or key", rawURL)
	}
	return bucket, key, nil
}

// Fetch opens a streaming GET of the S3 object at ref.URL. The returned body
// must be drained and closed by the caller. contentLength is 0 when the SDK
// response omits Content-Length (resp.ContentLength is nil).
//
// creds must be one of:
//   - s3Credentials — concrete type from parseAuth / parseS3Auth
//   - rawAuthCreds — raw payload arriving before this source was wired; decoded here
//   - noCreds — rejected; S3 requires signing credentials
//
// Any other Credentials implementation is rejected with an incompatible-kind error.
func (s *s3Source) Fetch(ctx context.Context, ref Reference, creds Credentials) (io.ReadCloser, int64, error) {
	bucket, key, err := parseS3URL(ref.URL)
	if err != nil {
		return nil, 0, err
	}

	var c s3Credentials
	switch v := creds.(type) {
	case s3Credentials:
		c = v
	case rawAuthCreds:
		// Payload arrived via the generic parseAuth path; decode inline.
		c, err = parseS3Auth(v.Raw)
		if err != nil {
			return nil, 0, err
		}
	case noCreds:
		return nil, 0, fmt.Errorf("stemcell_fetch(s3): credentials required (none provided)")
	default:
		return nil, 0, fmt.Errorf("stemcell_fetch(s3): incompatible credentials kind %q", creds.Kind())
	}

	region := c.Region
	if region == "" {
		region = "us-east-1"
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(s3): load AWS config: %w", err)
	}

	var opts []func(*s3.Options)
	if c.Endpoint != "" {
		endpoint := c.Endpoint // capture for closure
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true // MinIO/R2 require path-style when endpoint is overridden
		})
	} else if c.ForcePathStyle {
		opts = append(opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(cfg, opts...)
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("stemcell_fetch(s3): GetObject s3://%s/%s: %w", bucket, key, err)
	}

	var size int64
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	return resp.Body, size, nil
}
