package stemcellfetch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// Credentials applies authentication to a Source's outbound request. Each
// scheme has its own concrete implementation: basicCreds, bearerCreds,
// s3Creds, blobstoreCreds, ociCreds.
//
// Apply is called by HTTPS-style sources with a prepared *http.Request.
// S3/OCI sources may type-assert to richer scheme-specific interfaces —
// Credentials is the common minimum contract.
type Credentials interface {
	// Apply attaches auth material to req. May be a no-op for sources that
	// use a signing client rather than HTTP headers (e.g. AWS SigV4 via SDK).
	Apply(req *http.Request) error

	// Kind reports the auth scheme (basic|bearer|s3|blobstore|oci|none) for
	// logging and diagnostics. Never returns sensitive values.
	Kind() string
}

// noCreds is the unauthenticated placeholder returned when no credential
// defaults match and no per-stemcell auth is provided. Apply is a no-op.
// Callers that warn on unauthenticated fetches check Kind() == "none".
type noCreds struct{}

func (noCreds) Apply(_ *http.Request) error { return nil }
func (noCreds) Kind() string                { return "none" }

// authEnvelope is the JSON discriminator on auth payloads. Only the "type"
// field is decoded here; concrete types are decoded in parseAuth.
type authEnvelope struct {
	Type string `json:"type"`
}

// ResolveCredentials returns Credentials for url using the standard
// resolution order:
//
//  1. propsAuth (per-stemcell cloud_properties.image_url_auth) — overrides all
//  2. Longest URLPrefix match in defaults
//  3. noCreds (unauthenticated); caller may emit a warning
//
// Returns (nil, error) only on malformed auth payloads. A no-match condition
// returns (noCreds{}, nil) so the caller decides whether to warn or proceed.
//
// Input invariants:
//   - propsAuth: nil or empty or "null" → treated as absent
//   - defaults: nil slice is valid (no configured defaults)
//   - url: empty string results in no prefix match → noCreds
func ResolveCredentials(propsAuth json.RawMessage, defaults []config.FetchCredentialDefault, url string) (Credentials, error) {
	// Per-stemcell override takes priority.
	if len(propsAuth) > 0 && string(propsAuth) != "null" {
		return parseAuth(propsAuth)
	}

	// Longest-prefix match against configured defaults.
	matched := longestPrefixMatch(defaults, url)
	if matched != nil {
		return parseAuth(matched.Auth)
	}

	// No match — unauthenticated.
	return noCreds{}, nil
}

// longestPrefixMatch scans defaults for the entry whose URLPrefix is a
// prefix of url; among all matching entries the one with the longest
// URLPrefix wins. Returns nil when no entry matches.
//
// Entries with empty URLPrefix are skipped (the config Validate pass
// rejects them, but defensive skipping here prevents an empty-prefix
// from matching every URL).
func longestPrefixMatch(defaults []config.FetchCredentialDefault, url string) *config.FetchCredentialDefault {
	var best *config.FetchCredentialDefault
	bestLen := -1
	for i := range defaults {
		d := &defaults[i]
		if d.URLPrefix == "" {
			continue
		}
		if strings.HasPrefix(url, d.URLPrefix) && len(d.URLPrefix) > bestLen {
			best = d
			bestLen = len(d.URLPrefix)
		}
	}
	return best
}

// parseAuth dispatches on the "type" field of the auth payload and returns
// the concrete Credentials. Implemented types: basic, bearer, s3, blobstore, oci.
//
// Failure modes:
//   - empty/null payload → noCreds (defensive; callers screen this upstream)
//   - JSON unmarshal error → error
//   - missing "type" field → error
//   - unknown type string → error
//   - s3: missing access_key_id or secret_access_key → error from parseS3Auth
//   - blobstore: missing endpoint → error from parseBlobstoreAuth
//   - oci: malformed JSON → error from parseOCIAuth; absent creds → ociCredentials{}
func parseAuth(raw json.RawMessage) (Credentials, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return noCreds{}, nil
	}

	var env authEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("stemcell_fetch: parse auth envelope: %w", err)
	}

	switch env.Type {
	case "basic":
		var bc basicCreds
		if err := json.Unmarshal(raw, &bc); err != nil {
			return nil, fmt.Errorf("stemcell_fetch: parse basic auth: %w", err)
		}
		if bc.Username == "" {
			return nil, fmt.Errorf("stemcell_fetch: basic auth missing required field \"username\"")
		}
		return bc, nil

	case "bearer":
		var br bearerCreds
		if err := json.Unmarshal(raw, &br); err != nil {
			return nil, fmt.Errorf("stemcell_fetch: parse bearer auth: %w", err)
		}
		if br.BearerToken == "" {
			return nil, fmt.Errorf("stemcell_fetch: bearer auth missing required field \"bearer_token\"")
		}
		return br, nil

	case "s3":
		return parseS3Auth(raw)

	case "blobstore":
		return parseBlobstoreAuth(raw)

	case "oci":
		return parseOCIAuth(raw)

	case "":
		return nil, fmt.Errorf("stemcell_fetch: auth payload missing required field \"type\"")

	default:
		return nil, fmt.Errorf("stemcell_fetch: unknown auth type %q (supported: basic, bearer, s3, blobstore, oci)", env.Type)
	}
}

// basicCreds implements HTTP Basic auth for https:// sources.
type basicCreds struct {
	// Type field present in JSON but not used at runtime after dispatch.
	Username string `json:"username"`
	Password string `json:"password"`
}

func (b basicCreds) Apply(req *http.Request) error {
	req.SetBasicAuth(b.Username, b.Password)
	return nil
}

func (basicCreds) Kind() string { return "basic" }

// bearerCreds implements HTTP Bearer token auth for https:// sources.
type bearerCreds struct {
	// Type field present in JSON but not used at runtime after dispatch.
	BearerToken string `json:"bearer_token"`
}

func (b bearerCreds) Apply(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+b.BearerToken)
	return nil
}

func (bearerCreds) Kind() string { return "bearer" }

// rawAuthCreds carries a raw JSON auth payload for sources that decode
// credentials themselves rather than via HTTP header injection. Apply is
// intentionally a no-op: concrete sources type-assert Credentials to
// rawAuthCreds and decode Raw using scheme-specific logic.
type rawAuthCreds struct {
	authType string
	Raw      json.RawMessage
}

func (r rawAuthCreds) Apply(_ *http.Request) error { return nil }
func (r rawAuthCreds) Kind() string                { return r.authType }
