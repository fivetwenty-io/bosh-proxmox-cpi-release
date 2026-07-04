package log

import (
	"regexp"
	"strings"
)

// RedactedPlaceholder is the string substituted for every value the redactor
// classifies as sensitive. It is exported so callers (and tests) can assert on
// it without re-declaring the literal.
const RedactedPlaceholder = "<redacted>"

// sensitiveKeyFragments are lowercased substrings that, when contained in a map
// key, mark that key's entire value as a secret regardless of value type. They
// are matched as substrings (not exact names) so prefixed and suffixed variants
// — nats_password, db_password, client_secret, secret_access_key, refresh_token
// — are all caught without enumerating every spelling. The list deliberately
// errs toward over-redaction: a debug trace is a diagnostic aid, and masking a
// non-secret is harmless where leaking a credential is not.
var sensitiveKeyFragments = []string{
	"password",
	"passwd",
	"passphrase",
	"secret",
	"token",
	"credential",
	"mbus",
	"private_key",
	"privatekey",
	"access_key", // AWS secret_access_key and access_key_id
	"apikey",
	"api_key",
	"authorization",
	"signature", // S3 X-Amz-Signature, legacy query Signature
}

// sensitiveExactKeys are lowercased keys masked on an exact match only. "user"
// and "username" name a credential's other half, but substring-matching "user"
// would also clobber operationally useful, non-secret keys such as user_data
// (the cloud-init blob an operator raises debug to inspect) and user_agent.
// Exact match catches the registry/blobstore user field without that collateral.
var sensitiveExactKeys = map[string]struct{}{
	"user":     {},
	"username": {},
}

// urlUserinfo masks the credentials embedded in a URL's userinfo segment
// (scheme://user:pass@host, as a BOSH mbus URL carries). It is intentionally
// NOT anchored so a URL appearing mid-string or with leading whitespace is still
// scrubbed. The userinfo run is matched greedily up to the LAST "@" before the
// path ([^/\s]+ admits "@"), so a password containing a raw, un-encoded "@" — as
// BOSH-generated mbus/NATS credentials do — is masked in full rather than only
// up to its first "@". The "@" must still precede any "/", so a path-embedded
// "@" in a credential-free URL does not match.
var urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s]+@`)

// sensitiveQueryParam masks the value of a URL query parameter whose name either
// contains a sensitive fragment (?access_token=, ?password=, ?X-Amz-Signature=
// via "signature") or is exactly "sig" (the Azure SAS signature parameter, too
// short to substring-match without clobbering benign names like "design"). The
// fragment alternation is built once from sensitiveKeyFragments so the key-name
// and query-name secret vocabularies stay in sync. Case-insensitive; the
// captured prefix (delimiter + name + "=") is preserved and only the value is
// replaced.
var sensitiveQueryParam = buildSensitiveQueryParamRegexp()

func buildSensitiveQueryParamRegexp() *regexp.Regexp {
	escaped := make([]string, len(sensitiveKeyFragments))
	for i, frag := range sensitiveKeyFragments {
		escaped[i] = regexp.QuoteMeta(frag)
	}
	alt := strings.Join(escaped, "|")
	// A name is sensitive when it CONTAINS a fragment, or is one of the short
	// exact tokens that cannot be substring-matched safely.
	substrName := `[^=&#\s]*(?:` + alt + `)[^=&#\s]*`
	const exactName = `sig`
	return regexp.MustCompile(`(?i)([?&](?:` + substrName + `|` + exactName + `)=)[^&#\s]+`)
}

// RedactSecrets returns a deep copy of tree with every value under a sensitive
// key replaced by RedactedPlaceholder and every credential embedded in a URL
// userinfo segment masked. Map and slice structure is preserved; the input is
// never mutated (no map or slice from tree is aliased into the result), so a
// caller may safely log the result while continuing to use the original.
//
// It is intended for the CPI argument and result trees (decoded from JSON into
// map[string]any / []any / scalars). Inputs that are not maps, slices, or
// strings pass through unchanged. RedactSecrets is idempotent: applying it to an
// already-redacted tree yields an equal tree.
func RedactSecrets(tree any) any {
	switch t := tree.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			if keyIsSensitive(k) {
				out[k] = RedactedPlaceholder
				continue
			}
			out[k] = RedactSecrets(v)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = RedactSecrets(v)
		}
		return out
	case string:
		return scrubURLString(t)
	default:
		// Numbers, bools, nil, and any other scalar carry no key context and
		// cannot themselves be a URL credential — return as-is.
		return tree
	}
}

// keyIsSensitive reports whether a map key names a secret, by exact match
// against sensitiveExactKeys or case-insensitive substring match against
// sensitiveKeyFragments.
func keyIsSensitive(key string) bool {
	lower := strings.ToLower(key)
	if _, ok := sensitiveExactKeys[lower]; ok {
		return true
	}
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// scrubURLString masks credentials carried inside a URL-shaped string value,
// leaving any non-URL string unchanged. It catches secrets embedded under a key
// whose name is not itself sensitive (a blobstore or registry endpoint carrying
// either user:pass@ userinfo or a ?token=/?password= query parameter). Both
// forms are masked; an ordinary credential-free URL is returned untouched.
func scrubURLString(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+RedactedPlaceholder+"@")
	s = sensitiveQueryParam.ReplaceAllString(s, "${1}"+RedactedPlaceholder)
	return s
}

// ScrubMessage returns s with URL-embedded credentials masked (userinfo and
// sensitive query parameters), leaving credential-free text unchanged. Use it
// when a string derived from a guest-controlled or PVE-returned value (an
// error message, a span status) leaves the process by a path that does not go
// through ErrScrubbed — every external sink must apply the same scrubbing the
// logs do.
func ScrubMessage(s string) string {
	return scrubURLString(s)
}
