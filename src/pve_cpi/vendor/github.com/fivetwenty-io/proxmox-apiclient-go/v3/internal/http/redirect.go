package http

import (
	"net/http"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/redirect"
)

// ErrRedirectRefused is matched by errors.Is on the error the client returns
// when a server answers a request with a redirect and the client is not set to
// follow it. It is the default, because Go's own policy follows up to ten
// redirects and keeps the Authorization header whenever the host name stays the
// same, even when the redirect changes only the port or the scheme, which would
// send an API token to another listener on the host, in plain text for an http
// target.
var ErrRedirectRefused = redirect.ErrRefused

// RedirectError describes a redirect the client refused to follow. It matches
// ErrRedirectRefused, and the retry middleware never retries it.
type RedirectError = redirect.Error

// newHTTPClient builds the http.Client requests are sent with. When options
// leave CheckRedirect nil, the transport turns every redirect response into a
// refusal before net/http reads its Location header, and the client's own
// policy is the refusal as well. When options set a policy, that policy alone
// decides and the transport is left as it is.
func newHTTPClient(options *Options, roundTripper http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     redirect.Guard(roundTripper, options.CheckRedirect),
		Timeout:       options.Timeout,
		CheckRedirect: redirect.Wrap(options.CheckRedirect),
	}
}
