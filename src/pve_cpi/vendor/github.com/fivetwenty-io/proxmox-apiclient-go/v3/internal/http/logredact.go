package http

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/safetext"
)

// Fields that stand in for a body, or for the part of it a log line leaves out.
const (
	logFieldBody            = "resp_body"
	logFieldBodyLength      = "resp_body_length"
	logFieldBodyContentType = "resp_body_content_type"

	// truncatedSuffix marks a logged body cut at MaxBodyBytes.
	truncatedSuffix = "...(truncated)"

	// tokenCreateMarker is the key that identifies the response to a token
	// create call, whose "value" member is the token secret.
	tokenCreateMarker = "full-tokenid"
	tokenSecretMember = "value"

	// dataMember is the member of a PVE reply that holds the result.
	dataMember = "data"

	// maxRedactBodyBytes is the largest body the redactor parses. A larger
	// body is logged by its length and content type, because decoding and
	// encoding a body costs many times its size in memory, and a debug log
	// line is not worth that.
	maxRedactBodyBytes = 64 * 1024

	// Names of the headers that carry a URL.
	headerLocation        = "location"
	headerContentLocation = "content-location"
)

// secretNameFragments are the parts of a name that mark its value as a secret.
// A query parameter, a header, or a JSON member whose lower-cased name holds
// one is redacted. They are matched as plain substrings, which redacts more
// than strictly needed (a name such as "monkey" matches "key") and never less,
// except for otpFragments and benignNames below.
func secretNameFragments() []string {
	return []string{
		redactParamPassword, "passwd", redactParamSecret, redactParamToken, "ticket", "key", "authkey", "recovery",
		"challenge", "csrf", "cookie", "credential", "private",
	}
}

// otpFragments mark a one-time password. A plain substring match would redact
// "hotplug" and "footprint", so these match only at the start or the end of a
// word of the name, where a word is a run of letters.
func otpFragments() []string {
	return []string{"otp", "totp"}
}

// benignNames are PVE option names that hold a secret fragment and no secret.
// They are removed from a name before the fragment test.
func benignNames() []string {
	return []string{"keyboard"}
}

// alwaysRedactedHeaders are the headers whose values never reach a log line,
// whatever the configured list says. They carry credentials in either
// direction.
func alwaysRedactedHeaders() []string {
	return []string{headerAuthorization, "proxy-authorization", headerCookie, "set-cookie", headerCSRF}
}

// alwaysRedactedParams are the query parameter and JSON member names whose
// values never reach a log line, whatever the configured list says.
func alwaysRedactedParams() []string {
	return []string{
		redactParamPassword, redactParamToken, redactParamSecret, "ticket", "vncticket", headerCSRF, "otp",
		"privatekey", "private-key", headerAuthorization, headerCookie,
	}
}

// nameMatcher decides whether the value behind a name is a secret. It holds
// the exact names a caller or the SDK listed, compared without regard to case,
// and it also redacts any name that holds one of secretNameFragments.
type nameMatcher map[string]struct{}

// newNameMatcher returns a matcher for the lower-cased names in the lists.
func newNameMatcher(lists ...[]string) nameMatcher {
	set := make(nameMatcher)

	for _, list := range lists {
		for _, name := range list {
			set[strings.ToLower(name)] = struct{}{}
		}
	}

	return set
}

// secret reports whether the value behind name must be redacted.
func (m nameMatcher) secret(name string) bool {
	lower := strings.ToLower(name)

	if _, listed := m[lower]; listed {
		return true
	}

	return holdsSecretFragment(lower)
}

// holdsSecretFragment reports whether the lower-cased name holds a fragment of
// a secret's name.
func holdsSecretFragment(lower string) bool {
	checked := lower
	for _, benign := range benignNames() {
		checked = strings.ReplaceAll(checked, benign, "")
	}

	for _, fragment := range secretNameFragments() {
		if strings.Contains(checked, fragment) {
			return true
		}
	}

	words := strings.FieldsFunc(checked, func(r rune) bool { return r < 'a' || r > 'z' })
	for _, word := range words {
		for _, fragment := range otpFragments() {
			if strings.HasPrefix(word, fragment) || strings.HasSuffix(word, fragment) {
				return true
			}
		}
	}

	return false
}

// redactedURL renders target for a log line or a hook. It keeps the scheme, host,
// and path, drops the user information and the fragment, and, when the
// configuration logs query parameters, keeps the query with the value of every
// redacted parameter replaced. It is what the query field shows, in the form of
// the URL, so the two never disagree.
func redactedURL(target *url.URL, cfg LogConfig) string {
	if target == nil {
		return ""
	}

	out := url.URL{Scheme: target.Scheme, Host: target.Host, Path: target.Path, RawPath: target.RawPath}

	if cfg.LogQueryParams {
		out.RawQuery = redactRawQuery(target.RawQuery, newNameMatcher(cfg.RedactParams, alwaysRedactedParams()))
	}

	return out.String()
}

// redactRawQuery replaces the value of each parameter whose decoded name is in
// names. A parameter whose name does not decode is redacted too, because
// nothing can show it is harmless. The order and the spelling of the rest are
// kept.
func redactRawQuery(raw string, names nameMatcher) string {
	if raw == "" {
		return ""
	}

	pairs := strings.Split(raw, "&")

	for i, pair := range pairs {
		name, _, hasValue := strings.Cut(pair, "=")
		if !hasValue {
			continue
		}

		decoded, err := url.QueryUnescape(name)
		if err != nil {
			pairs[i] = "REDACTED_NAME=" + redactedValue

			continue
		}

		if names.secret(decoded) {
			pairs[i] = name + "=" + url.QueryEscape(redactedValue)
		}
	}

	return strings.Join(pairs, "&")
}

// bodyFields returns the log fields for a response body. A body that parses as
// JSON is logged with the value of every secret member replaced, with control
// and text direction characters escaped, cut at limit bytes when limit is
// positive. A body that does not parse, is a bare scalar, or is larger than
// maxRedactBodyBytes, is not logged: its length and content type stand in for it, because text
// that is not JSON gives nothing to tell a secret from the rest, and a large
// body is not parsed at all.
func bodyFields(body []byte, contentType string, cfg LogConfig) map[string]interface{} {
	if len(body) > maxRedactBodyBytes {
		return map[string]interface{}{
			logFieldBodyLength:      len(body),
			logFieldBodyContentType: contentType,
		}
	}

	redacted, ok := redactJSONBody(body, newNameMatcher(cfg.RedactParams, cfg.RedactHeaders, alwaysRedactedParams()))
	if !ok {
		return map[string]interface{}{
			logFieldBodyLength:      len(body),
			logFieldBodyContentType: contentType,
		}
	}

	redacted = []byte(safetext.Line(string(redacted)))

	if cfg.MaxBodyBytes > 0 && len(redacted) > cfg.MaxBodyBytes {
		cut := cfg.MaxBodyBytes
		for cut > 0 && !utf8.RuneStart(redacted[cut]) {
			cut--
		}

		redacted = append(redacted[:cut:cut], truncatedSuffix...)
	}

	return map[string]interface{}{logFieldBody: string(redacted)}
}

// redactJSONBody parses body as one JSON value and returns it re-encoded with
// the value of each member named in names replaced. It reports false when body
// is not exactly one JSON object or array, because a string, number, or other
// scalar on its own has no name to tell a secret from the rest. A string that
// is the "data" member of the top-level object is replaced too, for the same
// reason, since PVE puts a bare result there and nothing names it.
func redactJSONBody(body []byte, names nameMatcher) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var value interface{}

	err := decoder.Decode(&value)
	if err != nil {
		return nil, false
	}

	// Anything after the first value makes the body not one JSON document.
	if decoder.More() {
		return nil, false
	}

	switch typed := value.(type) {
	case map[string]interface{}:
		redactBareData(typed)
	case []interface{}:
		// An array has no member to name, and redactJSONValue walks its items.
	default:
		return nil, false
	}

	var out bytes.Buffer

	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)

	err = encoder.Encode(redactJSONValue(value, names))
	if err != nil {
		return nil, false
	}

	return bytes.TrimRight(out.Bytes(), "\n"), true
}

// redactBareData replaces a string held by the "data" member of the top-level
// object, ignoring case. The member is the result of the call, and a string
// there carries no name, so nothing can show it is not a secret.
func redactBareData(object map[string]interface{}) {
	for name, member := range object {
		if !strings.EqualFold(name, dataMember) {
			continue
		}

		if _, isString := member.(string); isString {
			object[name] = redactedValue
		}
	}
}

// redactJSONValue returns value with the value of each object member named in
// names replaced, and every string item of an array replaced, at any depth. In
// an object that carries the member a token create response uses to name the
// new token, the "value" member is the token secret and is replaced as well.
func redactJSONValue(value interface{}, names nameMatcher) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		_, isTokenCreate := lookupFold(typed, tokenCreateMarker)
		out := make(map[string]interface{}, len(typed))

		for key, member := range typed {
			lower := strings.ToLower(key)

			if names.secret(key) || (isTokenCreate && lower == tokenSecretMember) {
				out[key] = redactedValue

				continue
			}

			out[key] = redactJSONValue(member, names)
		}

		return out
	case []interface{}:
		out := make([]interface{}, len(typed))

		for i, member := range typed {
			// An array item has no member name to tell a secret from the rest, so
			// a string in one is replaced as the "data" string is.
			if _, isString := member.(string); isString {
				out[i] = redactedValue

				continue
			}

			out[i] = redactJSONValue(member, names)
		}

		return out
	default:
		return value
	}
}

// lookupFold finds key in object ignoring case.
func lookupFold(object map[string]interface{}, key string) (interface{}, bool) {
	for name, member := range object {
		if strings.EqualFold(name, key) {
			return member, true
		}
	}

	return nil, false
}

// redactResponseHeaders returns the response headers for a log line. The
// credential headers are replaced, the value of Location and Content-Location
// goes through the same URL redaction as the url field, and every other value
// has control and text direction characters escaped, because a server chose it.
func redactResponseHeaders(header map[string][]string, cfg LogConfig) map[string]interface{} {
	out := redact(header, cfg.RedactHeaders, alwaysRedactedHeaders()...)

	for key, value := range out {
		values, ok := value.([]string)
		if !ok {
			continue
		}

		lower := strings.ToLower(key)
		isURL := lower == headerLocation || lower == headerContentLocation

		for i, text := range values {
			if isURL {
				values[i] = redactedLocation(text, cfg)
			} else {
				values[i] = safetext.Line(text)
			}
		}
	}

	return out
}

// redactedLocation renders the value of a Location header like the url field. A
// value that is not a URL is replaced.
func redactedLocation(value string, cfg LogConfig) string {
	target, err := url.Parse(value)
	if err != nil {
		return redactedValue
	}

	return safetext.Line(redactedURL(target, cfg))
}
