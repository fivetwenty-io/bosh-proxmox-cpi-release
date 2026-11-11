package http

import (
	"bytes"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
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

	// pairedValueMember is the member that holds the value of a name given by
	// a sibling member. A token create reply names the token in a sibling
	// ("full-tokenid" on PVE, "tokenid" on PBS and PDM) and puts the secret
	// here, and a pending config entry names the option in a "key" sibling.
	pairedValueMember = "value"

	// pairedPendingMember is the member of a pending config entry that holds
	// the setting not yet applied, beside the current one in "value". The
	// pending config of a VM, a container, and a cloud-init drive all use it.
	pairedPendingMember = "pending"

	// namingNameMember and namingKeyMember are the members whose string value
	// names the setting a paired value member holds. The Ceph config database
	// gives the option name in "name", and a pending config entry gives it in
	// "key".
	namingNameMember = "name"
	namingKeyMember  = "key"

	// dataMember is the member of a PVE reply that holds the result, and the
	// member of an ACME plugin that holds the DNS provider's credentials.
	dataMember = "data"

	// redactedHeaderValueBytes is the most bytes of escaped text a log line
	// keeps from one response header name or value, or from the content type.
	redactedHeaderValueBytes = safetext.MaxServerTextBytes

	// maxLoggedHeaderValues is the most values of one response header a log
	// line keeps. A header can repeat as often as the transport's header limit
	// allows.
	maxLoggedHeaderValues = 16

	// maxLoggedHeaderBytes is the most bytes of header names and values the
	// resp_headers field holds, which is the transport's header limit cut
	// down to what a debug log line is worth. It holds one header with every
	// value it may keep, at the longest each value may be, with room to spare.
	maxLoggedHeaderBytes = 32 * 1024

	// droppedHeadersField counts the response headers left out of the
	// resp_headers field. Parentheses cannot appear in a header name, so no
	// header the server sends can take this name.
	droppedHeadersField = "(dropped headers)"

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
		return bodyStandIn(body, contentType)
	}

	redacted, ok := redactJSONBody(body, newNameMatcher(cfg.RedactParams, cfg.RedactHeaders, alwaysRedactedParams()))
	if !ok {
		return bodyStandIn(body, contentType)
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

// bodyStandIn returns the fields logged in place of a body, which are its
// length and its content type. The server chose the content type, so it is
// escaped and cut like any other header value.
func bodyStandIn(body []byte, contentType string) map[string]interface{} {
	return map[string]interface{}{
		logFieldBodyLength:      len(body),
		logFieldBodyContentType: safetext.Capped(contentType, redactedHeaderValueBytes),
	}
}

// redactJSONBody parses body as one JSON value and returns it re-encoded with
// the value of each member named in names replaced. It reports false when body
// is not exactly one JSON object or array, because a string, number, or other
// scalar on its own has no name to tell a secret from the rest.
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

	switch value.(type) {
	case map[string]interface{}, []interface{}:
		// redactJSONValue walks the members and the items.
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

// redactJSONValue returns value with the value of each object member named in
// names replaced, and every string item of an array replaced, at any depth.
//
// Two members are replaced by their place and not their name. A string held by
// a "data" member is the result of a call, or the credentials of an ACME
// plugin, and carries no name that shows it is harmless. A "value" or
// "pending" member whose object also holds a member named as a secret, such as
// the "tokenid" or "full-tokenid" of a token create reply or the "key" of a
// pending config entry, holds the value that name stands for. So does one whose
// object has a "name" or "key" member whose value is the name of a secret, such
// as the "name" of a Ceph config option.
func redactJSONValue(value interface{}, names nameMatcher) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		paired := holdsSecretSibling(typed, names)
		out := make(map[string]interface{}, len(typed))

		for key, member := range typed {
			lower := strings.ToLower(key)
			_, isString := member.(string)

			if names.secret(key) || (paired && isPairedValue(lower)) || (isString && lower == dataMember) {
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

// holdsSecretSibling reports whether object has a member, other than the
// paired value members themselves, whose name marks a secret, or a naming
// member whose string value is the name of a secret. A Ceph config option
// such as {"name":"mgr/influx/password","value":...} names its setting in the
// value of "name", and no member name there marks a secret.
func holdsSecretSibling(object map[string]interface{}, names nameMatcher) bool {
	for name, member := range object {
		lower := strings.ToLower(name)
		if isPairedValue(lower) {
			continue
		}

		if names.secret(name) {
			return true
		}

		setting, isString := member.(string)
		if isString && isNamingMember(lower) && names.secret(setting) {
			return true
		}
	}

	return false
}

// isNamingMember reports whether a lower-cased member name is one whose value
// names the setting a paired value member holds, as "name" does in a Ceph
// config option and "key" does in a pending config entry.
func isNamingMember(lower string) bool {
	return lower == namingNameMember || lower == namingKeyMember
}

// isPairedValue reports whether a lower-cased member name is one that holds the
// current or the pending value of a name a sibling member gives.
func isPairedValue(lower string) bool {
	return lower == pairedValueMember || lower == pairedPendingMember
}

// redactResponseHeaders returns the response headers for a log line. The
// credential headers are replaced, the value of Location and Content-Location
// goes through the same URL redaction as the url field, and every other value
// has control and text direction characters escaped, because a server chose it.
//
// The server also chose how many headers there are, how long their names are,
// and how often each repeats, so each name and each value is cut at
// redactedHeaderValueBytes, a header keeps at most maxLoggedHeaderValues
// values, and the field holds at most maxLoggedHeaderBytes of names and values.
// Headers are taken in the order of their names, and those that no longer fit
// are counted under droppedHeadersField.
func redactResponseHeaders(header map[string][]string, cfg LogConfig) map[string]interface{} {
	redacted := redact(header, cfg.RedactHeaders, alwaysRedactedHeaders()...)

	names := make([]string, 0, len(redacted))
	for name := range redacted {
		names = append(names, name)
	}

	sort.Strings(names)

	out := make(map[string]interface{}, len(names))
	budget := maxLoggedHeaderBytes
	dropped := 0

	for _, name := range names {
		logged := safetext.Capped(name, redactedHeaderValueBytes)
		value, size := loggedHeaderValue(name, redacted[name], cfg)
		size += len(logged)

		// Two long names can share the part that survives the cut. The first
		// one keeps the field, and the other is counted as dropped.
		if _, taken := out[logged]; taken || size > budget {
			dropped++

			continue
		}

		budget -= size
		out[logged] = value
	}

	if dropped > 0 {
		out[droppedHeadersField] = dropped
	}

	return out
}

// loggedHeaderValue returns what a log line shows for the values of the header
// name, and how many bytes of text that holds. A redacted header stays
// redacted. Otherwise each value is redacted or escaped and cut, and values
// past maxLoggedHeaderValues are replaced by one entry that counts them.
func loggedHeaderValue(name string, value interface{}, cfg LogConfig) (interface{}, int) {
	values, ok := value.([]string)
	if !ok {
		return value, len(redactedValue)
	}

	lower := strings.ToLower(name)
	isURL := lower == headerLocation || lower == headerContentLocation

	kept := values
	if len(kept) > maxLoggedHeaderValues {
		kept = kept[:maxLoggedHeaderValues]
	}

	out := make([]string, 0, len(kept)+1)
	size := 0

	for _, text := range kept {
		if isURL {
			text = redactedLocation(text, cfg)
		} else {
			text = safetext.Capped(text, redactedHeaderValueBytes)
		}

		out = append(out, text)
		size += len(text)
	}

	if more := len(values) - len(kept); more > 0 {
		marker := "...(" + strconv.Itoa(more) + " more values)"
		out = append(out, marker)
		size += len(marker)
	}

	return out, size
}

// redactedLocation renders the value of a Location header like the url field. A
// value that is not a URL is replaced.
func redactedLocation(value string, cfg LogConfig) string {
	target, err := url.Parse(value)
	if err != nil {
		return redactedValue
	}

	return safetext.Capped(redactedURL(target, cfg), redactedHeaderValueBytes)
}
