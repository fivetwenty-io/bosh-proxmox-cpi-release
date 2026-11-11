package endpointpool

import (
	"fmt"
	"net/http"
)

// refuseRedirect returns ErrRedirectRefused for a request the HTTP client
// built to follow a redirect, and nil for every other request. Go sets
// Request.Response only on those follow-ups, so a request a caller built never
// carries it. The error names the host that sent the redirect and nothing else
// from either request.
func refuseRedirect(req *http.Request) error {
	if req == nil || req.Response == nil {
		return nil
	}
	from := "an unknown host"
	if prior := req.Response.Request; prior != nil && prior.URL != nil && prior.URL.Host != "" {
		from = prior.URL.Host
	}
	return fmt.Errorf("%w from %s", ErrRedirectRefused, from)
}

// redirectGuard is the round tripper RefuseRedirects returns.
type redirectGuard struct {
	base http.RoundTripper
}

// RefuseRedirects wraps base so that a request the HTTP client built to follow
// a redirect is refused with ErrRedirectRefused, and closes its body, before it
// reaches base. It serves a client that has no endpoint pool, where the pool's
// own refusal does not apply. Every other request goes to base unchanged. A nil
// base is a caller defect and makes the returned round tripper nil, which the
// SDK reports as an error from its constructor.
func RefuseRedirects(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		return nil
	}
	return &redirectGuard{base: base}
}

// RoundTrip refuses a redirect follow-up and sends anything else through the
// wrapped round tripper.
func (g *redirectGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := refuseRedirect(req); err != nil {
		closeRequestBody(req)
		return nil, err
	}
	return g.base.RoundTrip(req)
}

// CloseIdleConnections passes through to the wrapped round tripper when it has
// the method, so http.Client.CloseIdleConnections still reaches the transport.
func (g *redirectGuard) CloseIdleConnections() {
	if closer, ok := g.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
