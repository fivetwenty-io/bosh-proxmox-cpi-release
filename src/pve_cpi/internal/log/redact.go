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
	"auth",      // subsumes "authorization"; catches *_auth / auth_* spellings
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
// key replaced by RedactedPlaceholder and every credential inside a string
// value masked the way ScrubMessage masks it, except that PVE token and cookie
// values follow the narrower treePVECredential rule so the fingerprints built
// from this output stay stable. Map and slice structure is preserved; the input is
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
		return redactTreeString(t)
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

// pveCredential masks the value of a PVE API token header or auth cookie
// (PVEAPIToken=user@realm!id=secret, PVEAuthCookie=ticket), including a value
// set off by whitespace or a line break on either side of the "=" and a
// URL-encoded "%3D". Neither has a URL shape, so the userinfo and
// query-parameter rules never see them. The name needs only a non-letter
// before it, not a word boundary, because in URL-encoded text it follows a
// percent escape such as "%20" whose last hex digit is a word character. That
// character is captured and kept in the output. The SDK keeps both values out
// of its errors today; this rule is defence in depth for any text that echoes
// a request header.
var pveCredential = regexp.MustCompile(`(?i)(^|[^A-Za-z])(PVEAPIToken|PVEAuthCookie)(\s*(?:=|%3D))\s*\S+`)

// treePVECredential is the narrower PVE rule RedactSecrets applies. The
// allocation journal hashes RedactSecrets output into the fingerprints it
// persists and compares on a retry, so this rule must not change. A different
// one would make a retry against a generation opened under this rule fail its
// fingerprint check.
var treePVECredential = regexp.MustCompile(`(?i)\b(PVEAPIToken|PVEAuthCookie)(=|%3D)[ \t]*\S+`)

// scrubURLCredentials masks the credentials a URL can carry inside a string
// value: user:pass@ userinfo and a sensitive query parameter such as ?token=
// or ?password=. It catches secrets embedded under a key whose name is not
// itself sensitive, such as a blobstore or registry endpoint.
func scrubURLCredentials(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+RedactedPlaceholder+"@")
	return sensitiveQueryParam.ReplaceAllString(s, "${1}"+RedactedPlaceholder)
}

// scrubCredentials masks every credential shape the scrubbers know inside a
// string value, leaving credential-free text unchanged. The log fields and
// ScrubMessage share this rule set.
func scrubCredentials(s string) string {
	return pveCredential.ReplaceAllString(scrubURLCredentials(s), "${1}${2}${3}"+RedactedPlaceholder)
}

// redactTreeString masks the credentials inside a string value of an argument
// or result tree. It masks URL credentials exactly as scrubCredentials does
// but keeps the narrower treePVECredential rule, because fingerprints hash
// its output.
func redactTreeString(s string) string {
	return treePVECredential.ReplaceAllString(scrubURLCredentials(s), "${1}${2}"+RedactedPlaceholder)
}

// ScrubMessage returns s with URL-embedded credentials masked (userinfo and
// sensitive query parameters) and PVE token and cookie values masked, leaving
// credential-free text unchanged. Use it when a string derived from a
// guest-controlled or PVE-returned value (an error message, a span status)
// leaves the process by a path that does not go through ErrScrubbed. Every
// external sink applies the same scrubbing the logs do.
func ScrubMessage(s string) string {
	return scrubCredentials(s)
}
