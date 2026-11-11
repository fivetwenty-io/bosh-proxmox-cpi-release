// Package redirect holds the redirect policy every HTTP client the SDK builds
// shares. Go's own policy follows up to ten redirects and keeps the
// Authorization and Cookie headers whenever the host name stays the same, even
// when a redirect changes only the port or the scheme, which would send an API
// token or a ticket cookie to another listener on the host, in plain text for
// an http target. The SDK refuses every redirect unless the caller opts in.
//
// The refusal works at two layers. Guard wraps the transport under an
// http.Client and turns every redirect response into an error before net/http
// reads the Location header, because net/http parses that header before it
// calls the redirect policy and quotes it in the error when it is malformed.
// Scrub is the one place every error from a client's Do passes through, and it
// removes whatever net/http or the transport quoted from the response.
package redirect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/permanent"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/safetext"
)

// ErrRefused is matched by errors.Is on the error a client returns when a
// server answers a request with a redirect and the client is not set to follow
// it.
var ErrRefused = errors.New("redirect refused")

// Policy is the signature of http.Client.CheckRedirect.
type Policy = func(req *http.Request, via []*http.Request) error

// Error describes a redirect a client refused to follow. It matches ErrRefused
// and says it is permanent, so a retry loop does not repeat the request.
type Error struct {
	// Status is the status code of the redirect response, such as 302 or 307.
	// It is zero when the refusal comes from a redirect policy that saw no
	// response, or from an error net/http built without one.
	Status int

	// From is the URL that was requested, without user information, query,
	// or fragment. It is empty when it is not known. A URL longer than
	// MaxURLTextBytes is cut and ends with a truncation marker.
	From string

	// To is the URL the server asked for, without user information, query, or
	// fragment. It is empty when the response had no Location header or the
	// header could not be parsed. A URL longer than MaxURLTextBytes is cut and
	// ends with a truncation marker, because the server chose its length.
	To string

	// unparsed is set when the server answered with a redirect whose Location
	// header is not a valid URL, so there is no target to name.
	unparsed bool
}

// Error names the redirect target without any credential.
func (e *Error) Error() string {
	from := e.From
	if from == "" {
		from = "the server"
	}

	answered := "answered"
	if e.Status > 0 {
		answered = fmt.Sprintf("answered %d", e.Status)
	}

	switch {
	case e.unparsed:
		return fmt.Sprintf("redirect refused: %s %s with a Location header that is not a valid URL", from, answered)
	case e.To == "":
		return fmt.Sprintf("redirect refused: %s %s without a Location header it could use", from, answered)
	default:
		return fmt.Sprintf("redirect refused: %s %s and asked for %s", from, answered, e.To)
	}
}

// Is matches ErrRefused.
func (e *Error) Is(target error) bool { return target == ErrRefused }

// Permanent tells a retry loop not to repeat the request.
func (e *Error) Permanent() bool { return true }

// Refuse is the Policy the SDK installs unless the caller sets one. It refuses
// every redirect with an *Error. The hop it names as From is the last request
// sent, which is the one that received the redirect.
func Refuse(req *http.Request, via []*http.Request) error {
	refusal := &Error{To: redactBounded(req.URL)}

	if req.Response != nil {
		refusal.Status = req.Response.StatusCode
	}

	if len(via) > 0 {
		refusal.From = redactBounded(via[len(via)-1].URL)
	}

	return refusal
}

// Redact renders u as scheme, host, and path only, so neither a password in
// the user information nor a secret in the query or fragment reaches an error
// message.
func Redact(u *url.URL) string {
	if u == nil {
		return ""
	}

	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// MaxURLTextBytes is the most bytes of a URL that an error the package builds
// keeps. A server chooses the target of a redirect, and percent-encoding can
// triple the bytes of its path, so an error that named the whole URL could run
// to megabytes.
const MaxURLTextBytes = safetext.MaxServerTextBytes

// redactBounded renders u as Redact does and cuts the text at MaxURLTextBytes.
func redactBounded(u *url.URL) string {
	return safetext.Capped(Redact(u), MaxURLTextBytes)
}

// redactRawBounded renders raw as RedactRaw does and cuts the text at
// MaxURLTextBytes.
func redactRawBounded(raw string) string {
	return safetext.Capped(RedactRaw(raw), MaxURLTextBytes)
}

// RedactRaw does what Redact does for a URL held as text. Text that does not
// parse as a URL yields an empty string, because nothing in it can be trusted
// to hold no credential.
func RedactRaw(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	return Redact(u)
}

// Follow returns a Policy that follows at most maxRedirects redirects and then
// refuses with an *Error. It keeps Go's behavior of sending the Authorization
// and Cookie headers to a redirect target on the same host name, so it suits a
// caller who trusts every redirect its server sends. A negative maxRedirects
// counts as zero.
func Follow(maxRedirects int) Policy {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return Refuse(req, via)
		}

		return nil
	}
}

// FollowSameOrigin returns a Policy that follows at most maxRedirects
// redirects, and only while the target has the same scheme, host name, and port
// as the first request. Any other redirect is refused with an *Error. A
// negative maxRedirects counts as zero.
func FollowSameOrigin(maxRedirects int) Policy {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects || len(via) == 0 || !sameOrigin(via[0].URL, req.URL) {
			return Refuse(req, via)
		}

		return nil
	}
}

func sameOrigin(first, target *url.URL) bool {
	if first == nil || target == nil {
		return false
	}

	return strings.EqualFold(first.Scheme, target.Scheme) &&
		strings.EqualFold(first.Hostname(), target.Hostname()) &&
		effectivePort(first) == effectivePort(target)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}

	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}

	return "80"
}

// sameError reports whether two errors are the same pointer value. Scrub uses
// it to hand back the error it was given when nothing needs cleaning, and
// Wrap's callers rely on net/http comparing http.ErrUseLastResponse by
// identity, so == on the values is the contract and errors.Is is not.
func sameError(first, second error) bool {
	firstValue, secondValue := reflect.ValueOf(first), reflect.ValueOf(second)

	return firstValue.Kind() == reflect.Pointer && secondValue.Kind() == reflect.Pointer &&
		firstValue.Pointer() == secondValue.Pointer()
}

// Wrap returns the Policy to install in an http.Client. A nil policy becomes
// Refuse. Any other policy is returned as is, so http.ErrUseLastResponse still
// reaches net/http untouched, because net/http compares it by identity, and an
// error the policy returns comes back from Do for Scrub to clean.
func Wrap(policy Policy) Policy {
	if policy == nil {
		return Refuse
	}

	return policy
}

// Marker texts that identify an error whose message quotes a response header.
const (
	// unparsedLocationMarker is in the message net/http gives the error it
	// returns when the Location header of a redirect does not parse. The
	// message quotes the whole raw header, with its user information, query,
	// and fragment.
	unparsedLocationMarker = "failed to parse Location header"

	// malformedHeaderMarker is in the message the transport gives the error
	// it returns when a response header line is malformed. The message quotes
	// the whole raw line, which can be a Location, a Set-Cookie, or any other
	// header.
	malformedHeaderMarker = "malformed MIME header"

	// unsupportedSchemeMarker is in the message net/http gives the error it
	// returns when a request, which for a followed redirect is the server's
	// chosen target, names a scheme the transport cannot speak.
	unsupportedSchemeMarker = "unsupported protocol scheme"
)

// framingMarker names one kind of net/http or HTTP/2 protocol error whose
// message quotes bytes the server sent. The message is cut where the quoted
// part starts, so the kind stays readable and the bytes do not.
type framingMarker struct {
	// text is the phrase that starts the quoted part, or that names the kind.
	text string

	// keep is true when the phrase belongs to the kind and stays in the
	// message, and false when it only introduces the quoted bytes.
	keep bool

	// except is a phrase that, when present, shows the error describes the
	// caller's own request and not a response, so it is left alone.
	except string
}

// framingMarkers lists the protocol errors net/http and its HTTP/2 transport
// build from response bytes. Each quotes a status line, a status code, a
// version, a Content-Length, a Transfer-Encoding, a trailer key, a pseudo
// header, a header name, or the debug data of a GOAWAY frame. It is a function
// so that no package-level state can change the list.
func framingMarkers() []framingMarker {
	return []framingMarker{
		{text: "malformed HTTP response", keep: true},
		{text: "malformed HTTP status code", keep: true},
		{text: "malformed HTTP version", keep: true},
		{text: "invalid empty Content-Length", keep: true},
		{text: "bad Content-Length", keep: true},
		{text: "message cannot contain multiple Content-Length headers", keep: true},
		{text: "unsupported transfer encoding", keep: true},
		{text: "too many transfer encodings", keep: true},
		{text: "invalid Trailer key", keep: true},
		{text: "bad trailer key", keep: true},
		{text: "invalid pseudo-header", keep: true},
		{text: "duplicate pseudo-header", keep: true},
		{text: "invalid header field name", keep: true, except: "net/http: invalid header"},
		{text: "invalid header field value for", keep: true, except: "net/http: invalid header"},
		{text: ", debug=", keep: false},
	}
}

// FramingError describes a response the transport could not parse. The
// transport quotes the bytes it rejected in its own message, which come from
// the server, so the error keeps the kind of fault and drops the bytes from
// its text. It does not say it is permanent, so the retry loop treats it as it
// treated the original, because the retry goes to the same host with the same
// credentials. It unwraps to the original error, so errors.Is and errors.As
// find what they found before, and the retry decision for the cause does not
// change.
type FramingError struct {
	// Kind is the message of the original error up to the point where it
	// quotes the server's bytes.
	Kind string

	cause error
}

// Error returns the kind of fault without the bytes the server sent.
func (e *FramingError) Error() string { return e.Kind }

// Unwrap returns the original error, whose own text does quote the bytes. It
// exists so that errors.Is and errors.As see the whole chain. Code that prints
// the chain link by link prints those bytes, as it would have before.
func (e *FramingError) Unwrap() error { return e.cause }

// GoString keeps the %#v form to the kind of fault, because the default form
// would print the original error and with it the bytes the server sent.
func (e *FramingError) GoString() string {
	return fmt.Sprintf("&redirect.FramingError{Kind:%q}", e.Kind)
}

// HeaderError describes a response whose header or trailer the transport could
// not read. The transport quotes the whole raw header line in its own message,
// which can hold a redirect target with credentials or a cookie, so the error
// names only the requested URL. It is permanent, because the same server sends
// the same bytes on every attempt.
type HeaderError struct {
	// From is the URL that was requested, without user information, query,
	// or fragment. It is empty when it is not known.
	From string
}

// Error names the requested URL and nothing the server sent.
func (e *HeaderError) Error() string {
	from := e.From
	if from == "" {
		from = "the server"
	}

	return from + " answered with a header that could not be read"
}

// Permanent tells a retry loop not to repeat the request.
func (e *HeaderError) Permanent() bool { return true }

// Scrub is the one place every error from a client's Do passes through, for
// the client's own requests and for the ticket, pool, WebSocket, and batch
// paths. net/http wraps a failure in a *url.Error whose URL is the request URL
// with its user information masked but its query and fragment kept, which for
// a followed redirect is the server's chosen target. Scrub rebuilds that
// *url.Error with the URL reduced to scheme, host, and path.
//
// It also replaces a cause whose text quotes a response header. When the
// Location header of a followed redirect does not parse, net/http quotes it
// whole, and when the transport rejects a header line it quotes that line.
// The first becomes an *Error that names only the requested URL, like any
// other refusal, and the second becomes a *HeaderError. Both are permanent.
//
// A protocol error that net/http or its HTTP/2 transport builds from response
// bytes, such as a bad status code, a bad Content-Length, or an unsupported
// Transfer-Encoding, quotes those bytes. Scrub replaces it with a
// *FramingError that keeps the kind of fault and drops the bytes, and leaves
// the retry behavior as it was. A followed redirect to a scheme the transport
// cannot speak is marked permanent, because every attempt fails the same way.
//
// A *url.Error further down the chain, behind a wrapper's own %w or inside an
// errors.Join, is cleaned the same way. Its text is replaced inside the text of
// the error that holds it, and the result still unwraps to the error Scrub was
// given.
//
// Every other cause keeps its type and its chain but has its text escaped and
// cut at safetext.MaxErrorTextBytes, because a server can choose part of it,
// such as a name in the certificate it presents or the reason phrase a proxy
// gives. A URL in a *url.Error is cut at MaxURLTextBytes.
//
// An error that needs no change comes back as the same value.
func Scrub(err error) error {
	if err == nil {
		return nil
	}

	urlErr, ok := err.(*url.Error) //nolint:errorlint // only the error Do returns itself is rebuilt
	if !ok {
		return cleanCause(err, "")
	}

	from := redactRawBounded(urlErr.URL)
	cause := cleanCause(urlErr.Err, from)

	if from == urlErr.URL && sameCause(cause, urlErr.Err) {
		return err
	}

	return &url.Error{Op: urlErr.Op, URL: from, Err: cause}
}

// sameCause reports whether two causes are the same value, without comparing
// uncomparable dynamic types.
func sameCause(first, second error) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}

	return sameError(first, second)
}

// ScrubRead cleans an error from reading the body of resp, as Scrub cleans an
// error from Do. The body is read after Do returns, and a failure there can
// quote bytes the server sent, such as a malformed trailer line or the debug
// text of an HTTP/2 GOAWAY frame. The error names the URL of the request resp
// answers, or "the server" when resp carries no request.
func ScrubRead(err error, resp *http.Response) error {
	if err == nil {
		return nil
	}

	if urlErr, ok := err.(*url.Error); ok { //nolint:errorlint // only a bare *url.Error is rebuilt
		return Scrub(urlErr)
	}

	var requested *url.URL
	if resp != nil && resp.Request != nil {
		requested = resp.Request.URL
	}

	return cleanCause(err, redactBounded(requested))
}

// cleanCause returns cause, or a replacement that names only the requested URL
// from when the text of cause quotes a response header, or cause with its text
// escaped and cut.
func cleanCause(cause error, from string) error {
	if cause == nil {
		return nil
	}

	if nested, ok := cause.(*url.Error); ok { //nolint:errorlint // a nested rewrite applies to this value only
		return Scrub(nested)
	}

	// A *url.Error deeper in the chain is cleaned first, so no later step can
	// keep its query, and the classification below reads the cleaned text.
	cause = scrubNested(cause)

	// A certificate, context, or refusal error is never reclassified by its
	// text. A server can choose text inside such an error, such as a name in
	// the certificate it presents, and a match on that text would drop the
	// types that stop the retry loop. Its text is still escaped and cut.
	if isProtected(cause) {
		return boundText(cause)
	}

	text := cause.Error()

	// The markers below are phrases net/http and the transport write before
	// any bytes they quote. A protocol error that quotes the server, such as
	// the debug data of an HTTP/2 GOAWAY frame, can hold one of the phrases in
	// the part the server chose, so only the text before that part is read.
	own := text
	if start, _ := framingCut(text); start >= 0 {
		own = text[:start]
	}

	switch {
	case strings.Contains(own, unparsedLocationMarker):
		return &Error{From: from, unparsed: true}
	case strings.Contains(own, malformedHeaderMarker):
		return &HeaderError{From: from}
	case strings.Contains(own, unsupportedSchemeMarker) && !permanent.Is(cause):
		return permanent.Mark(boundText(cause)) //nolint:wrapcheck // marks cause, so it is the same error
	}

	kind, found := framingKind(text)
	if found {
		return &FramingError{Kind: safetext.Capped(kind, safetext.MaxServerTextBytes), cause: cause}
	}

	return boundText(cause)
}

// boundText returns err, or when its text holds a control character, a text
// direction override, or invalid UTF-8, or is longer than
// safetext.MaxErrorTextBytes, an error with the same chain whose text has those
// escaped and is cut.
func boundText(err error) error {
	return safetext.CappedError(err, safetext.MaxErrorTextBytes) //nolint:wrapcheck // same chain, text escaped
}

// maxChainLinks bounds how many links scrubNested visits, so an error whose
// Unwrap builds a new link on every call cannot keep it walking.
const maxChainLinks = 256

// nestedError is an error whose text is the text of the error it wraps with
// each *url.Error below the top cleaned. It unwraps to the error it was built
// from, so errors.Is and a link-by-link walk see what they saw. errors.As sees
// the same links too, except that a *url.Error it finds comes back cleaned.
// Code that unwraps past this error and reads a nested *url.Error itself still
// reaches the original, with its query, as it would have before.
type nestedError struct {
	text      string
	cause     error
	permanent bool
}

// Error returns the cleaned text.
func (e *nestedError) Error() string { return e.text }

// Unwrap returns the error whose text was cleaned.
func (e *nestedError) Unwrap() error { return e.cause }

// Permanent reports whether cleaning a nested *url.Error found a failure that
// stops the retry loop, such as a header the transport could not read, as it
// would have if the *url.Error had come back bare.
func (e *nestedError) Permanent() bool { return e.permanent }

// GoString keeps the %#v form to the cleaned text, because the default form
// would print the wrapped error and with it the URL.
func (e *nestedError) GoString() string { return "redirect.nestedError(" + strconv.Quote(e.text) + ")" }

// As finds what errors.As finds in the wrapped error. When that is a
// *url.Error, which a target of *url.Error or of an interface such as net.Error
// can match, it hands back the copy Scrub makes, so errors.As never returns the
// original with its query, user information, and fragment.
func (e *nestedError) As(target any) bool {
	if !errors.As(e.cause, target) {
		return false
	}

	slot := reflect.ValueOf(target).Elem()

	found, ok := slot.Interface().(*url.Error)
	if !ok {
		return true
	}

	scrubbed := reflect.ValueOf(Scrub(found))
	if scrubbed.Type().AssignableTo(slot.Type()) {
		slot.Set(scrubbed)
	}

	return true
}

// timeoutReporter is the Timeout half of net.Error.
type timeoutReporter interface{ Timeout() bool }

// temporaryReporter is the Temporary half of net.Error.
type temporaryReporter interface{ Temporary() bool }

// withNetMethods returns base, or base with a Timeout method, a Temporary
// method, or both, as its cause has them. A *url.Error asks its direct cause,
// so a timeout below the wrapper still reads as one, and errors.As for a
// net.Error stops at the wrapper only when it would have stopped at the cause.
func withNetMethods(base *nestedError) error {
	timeout, hasTimeout := base.cause.(timeoutReporter)
	temporary, hasTemporary := base.cause.(temporaryReporter)

	switch {
	case hasTimeout && hasTemporary:
		return &nestedNetError{nestedError: base, timeout: timeout, temporary: temporary}
	case hasTimeout:
		return &nestedTimeoutError{nestedError: base, timeout: timeout}
	case hasTemporary:
		return &nestedTemporaryError{nestedError: base, temporary: temporary}
	default:
		return base
	}
}

// nestedTimeoutError is a nestedError whose cause has a Timeout method and no
// Temporary method.
type nestedTimeoutError struct {
	*nestedError

	timeout timeoutReporter
}

// Timeout reports what the cause's Timeout method reports.
func (e *nestedTimeoutError) Timeout() bool { return e.timeout.Timeout() }

// nestedTemporaryError is a nestedError whose cause has a Temporary method and
// no Timeout method.
type nestedTemporaryError struct {
	*nestedError

	temporary temporaryReporter
}

// Temporary reports what the cause's Temporary method reports.
func (e *nestedTemporaryError) Temporary() bool { return e.temporary.Temporary() }

// nestedNetError is a nestedError whose cause has both methods, as a net.Error
// does, so the wrapper is a net.Error too.
type nestedNetError struct {
	*nestedError

	timeout   timeoutReporter
	temporary temporaryReporter
}

// Timeout reports what the cause's Timeout method reports.
func (e *nestedNetError) Timeout() bool { return e.timeout.Timeout() }

// Temporary reports what the cause's Temporary method reports.
func (e *nestedNetError) Temporary() bool { return e.temporary.Temporary() }

// scrubNested returns err, or when a *url.Error below the top of its chain
// needs cleaning, an error whose text has that *url.Error's text replaced by
// the text Scrub gives it. A wrapper that printed only the URL has the URL
// replaced by its redacted form. The walk follows Unwrap() error and Unwrap()
// []error, and stops at each *url.Error, because Scrub cleans what is below
// it.
func scrubNested(err error) error {
	text := err.Error()
	cleaned := text
	stopsRetry := false

	walkNested(err, func(nested *url.Error) {
		scrubbed := Scrub(nested)
		if sameError(scrubbed, nested) {
			return
		}

		stopsRetry = stopsRetry || permanent.Is(scrubbed)
		cleaned = strings.ReplaceAll(cleaned, nested.Error(), scrubbed.Error())

		redacted := redactRawBounded(nested.URL)
		if nested.URL != "" && nested.URL != redacted {
			cleaned = strings.ReplaceAll(cleaned, nested.URL, redacted)
		}
	})

	if cleaned == text && !stopsRetry {
		return err
	}

	return withNetMethods(&nestedError{text: cleaned, cause: err, permanent: stopsRetry})
}

// walkNested calls visit for each *url.Error in the chain of err below err
// itself, without looking below a *url.Error it found.
func walkNested(err error, visit func(*url.Error)) {
	queue := childrenOf(err)

	for seen := 0; len(queue) > 0 && seen < maxChainLinks; seen++ {
		link := queue[0]
		queue = queue[1:]

		if link == nil {
			continue
		}

		if nested, ok := link.(*url.Error); ok { //nolint:errorlint // the walk inspects each link itself
			visit(nested)

			continue
		}

		queue = append(queue, childrenOf(link)...)
	}
}

// childrenOf returns the errors err wraps directly.
func childrenOf(err error) []error {
	switch typed := err.(type) { //nolint:errorlint // the walk inspects each link itself
	case interface{ Unwrap() []error }:
		return typed.Unwrap()
	case interface{ Unwrap() error }:
		return []error{typed.Unwrap()}
	default:
		return nil
	}
}

// isProtected reports whether err is a TLS or x509 verification failure, a
// context error, or a redirect refusal anywhere in its chain. Their text can
// hold a name the server chose, and their types decide whether a request is
// retried, so cleanCause leaves them alone.
func isProtected(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRefused) {
		return true
	}

	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		return true
	}

	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}

	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}

	var invalidErr x509.CertificateInvalidError

	return errors.As(err, &invalidErr)
}

// framingKind cuts text at the point where a known protocol error starts to
// quote the server's bytes. It reports false when text holds no such error.
func framingKind(text string) (string, bool) {
	_, cut := framingCut(text)
	if cut < 0 {
		return "", false
	}

	return strings.TrimRight(text[:cut], " :;,"), true
}

// framingCut returns where the first known protocol error in text starts, and
// where its kind ends and the server's bytes begin, or -1 for both when text
// holds none. The marker that appears first in text wins, whatever its place
// in the list, because the quoted bytes that follow it can hold another marker
// phrase. A marker's except phrase is looked for only up to the end of the
// marker itself, because the bytes after it are the server's and could
// otherwise switch the marker off.
func framingCut(text string) (int, int) {
	first, cut := -1, -1

	for _, marker := range framingMarkers() {
		start := strings.Index(text, marker.text)
		if start < 0 || (first >= 0 && start >= first) {
			continue
		}

		end := start + len(marker.text)
		if marker.except != "" && strings.Contains(text[:end], marker.except) {
			continue
		}

		first, cut = start, start
		if marker.keep {
			cut = end
		}
	}

	return first, cut
}

// refusedStatus reports whether a response status is one Guard turns into a
// refusal. Every 3xx status counts except 304, which answers a conditional
// request with the copy the caller already holds and tells it to go nowhere.
// net/http does not follow a 304, and the PVE API does not use conditional
// requests, so passing it through refuses nothing a redirect could exploit and
// breaks a caller that sends If-None-Match through its own header.
func refusedStatus(status int) bool {
	return status >= http.StatusMultipleChoices && status < http.StatusBadRequest &&
		status != http.StatusNotModified
}

// refusingTransport is the RoundTripper Guard returns.
type refusingTransport struct {
	next http.RoundTripper
}

// Guard returns the RoundTripper to install under an http.Client. With a nil
// policy it wraps next so that every redirect response becomes a permanent
// *Error, with the response body closed, before net/http reads the Location
// header. Go's client parses that header before it calls CheckRedirect, and a
// header it cannot parse fails the request with an error that quotes the raw
// header, so a CheckRedirect policy alone cannot keep the header out of an
// error. With a policy the caller has chosen to follow redirects, so Guard
// returns next unchanged and the policy decides. A nil next means
// http.DefaultTransport, as it does for an http.Client.
//
// The wrapper sits above whatever transport the client uses, so a transport the
// caller wrapped still sees the redirect response and cannot let it through.
// It also rewrites a transport error that quotes a malformed header line to a
// *HeaderError, so a client used directly, such as the one Pool.Get returns,
// does not repeat the header either.
func Guard(next http.RoundTripper, policy Policy) http.RoundTripper {
	if policy != nil {
		return next
	}

	if next == nil {
		next = http.DefaultTransport
	}

	return &refusingTransport{next: next}
}

// Unwrap returns the transport Guard wrapped, or rt itself when rt is not the
// wrapper Guard returns. Code that tunes or inspects the transport an
// http.Client was built with reads it through Unwrap.
func Unwrap(rt http.RoundTripper) http.RoundTripper {
	if guard, ok := rt.(*refusingTransport); ok {
		return guard.next
	}

	return rt
}

// RoundTrip sends req and refuses a redirect answer.
func (t *refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, cleanCause(err, redactBounded(req.URL))
	}

	if resp == nil || !refusedStatus(resp.StatusCode) {
		return resp, nil
	}

	refusal := refusalFor(req, resp)

	if resp.Body != nil {
		_ = resp.Body.Close()
	}

	return nil, refusal
}

// CloseIdleConnections closes the idle connections of the wrapped transport,
// which http.Client.CloseIdleConnections reaches through this method.
func (t *refusingTransport) CloseIdleConnections() {
	closer, ok := t.next.(interface{ CloseIdleConnections() })
	if ok {
		closer.CloseIdleConnections()
	}
}

// refusalFor describes the redirect resp answers req with, naming the target
// as scheme, host, and path only. A Location header that does not parse, which
// can hold user information, a query, and a fragment, is not quoted.
func refusalFor(req *http.Request, resp *http.Response) *Error {
	refusal := &Error{Status: resp.StatusCode, From: redactBounded(req.URL)}

	location := resp.Header.Get("Location")
	if location == "" {
		return refusal
	}

	target, err := req.URL.Parse(location)
	if err != nil {
		refusal.unparsed = true

		return refusal
	}

	refusal.To = redactBounded(target)

	return refusal
}
