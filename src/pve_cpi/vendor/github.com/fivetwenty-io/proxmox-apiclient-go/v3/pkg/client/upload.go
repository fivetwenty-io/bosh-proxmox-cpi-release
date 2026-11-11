package client

import (
	"io"
	"net/http"

	pvehttp "github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/http"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/redirect"
)

// ErrHostOverrideFingerprint is returned when a request carries a WithHost
// override while TLS fingerprint pinning is enabled; see WithHost.
var ErrHostOverrideFingerprint = pvehttp.ErrHostOverrideFingerprint

// ErrWrapTransportNil is returned by NewClient when Options.WrapTransport
// returns a nil RoundTripper, or an interface holding a nil value; see
// Options.WrapTransport.
var ErrWrapTransportNil = pvehttp.ErrWrapTransportNil

// ErrRedirectRefused is matched by errors.Is on the error a request returns
// when the server answers with a redirect and Options.CheckRedirect is nil or
// does not allow it; see Options.CheckRedirect.
var ErrRedirectRefused = pvehttp.ErrRedirectRefused

// RedirectError is the error a refused redirect returns. It names the status
// and the two URLs without credentials, matches ErrRedirectRefused, and is
// never retried.
type RedirectError = pvehttp.RedirectError

// FollowRedirects returns an Options.CheckRedirect policy that follows at most
// maxRedirects redirects and then fails with a *RedirectError. It follows a
// redirect to any scheme, host, and port. Go keeps the Authorization and
// Cookie headers when only the port or the scheme changes, and it copies every
// other header, such as the CSRFPreventionToken header, to a different host as
// well, so use it only with a server whose redirects you trust. A negative
// maxRedirects counts as zero.
func FollowRedirects(maxRedirects int) func(req *http.Request, via []*http.Request) error {
	return redirect.Follow(maxRedirects)
}

// FollowSameOrigin returns an Options.CheckRedirect policy that follows at most
// maxRedirects redirects, and only to the same scheme, host name, and port as
// the original request. Any other redirect fails with a *RedirectError. A
// negative maxRedirects counts as zero.
func FollowSameOrigin(maxRedirects int) func(req *http.Request, via []*http.Request) error {
	return redirect.FollowSameOrigin(maxRedirects)
}

// Permanent marks err so the client's retry loop does not attempt the request
// again. A WrapTransport wrapper returns Permanent(err) from RoundTrip when it
// refuses a request on purpose, and a CheckRedirect function returns it for the
// same reason. errors.Is and errors.As still reach err. A nil err stays nil.
// An error type with a Permanent() bool method that returns true is treated the
// same way.
//
//nolint:wrapcheck // marks err, so it is the same error
func Permanent(err error) error { return pvehttp.Permanent(err) }

// IsPermanent reports whether err carries the permanent marker.
func IsPermanent(err error) bool { return pvehttp.IsPermanent(err) }

// NewSizedReader wraps r with its exact byte count so file uploads stream the
// multipart body with an explicit Content-Length instead of buffering the
// whole payload in memory to measure it. An *os.File needs no wrapping — its
// size is detected via Stat — but any other reader of known size should be
// wrapped before being passed to an upload method. The declared size must
// match the bytes r yields exactly: a short reader fails the upload, and
// surplus bytes are not read.
//
// A streamed upload body cannot be replayed by the client's internal retry or
// 401 re-authentication paths (the stream is consumed by the first attempt);
// callers needing retries should re-open the source and call the upload again.
func NewSizedReader(r io.Reader, size int64) io.Reader {
	return pvehttp.NewSizedReader(r, size)
}
