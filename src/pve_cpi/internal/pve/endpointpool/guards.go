package endpointpool

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// ErrUnsupportedBase is returned by New when the base transport carries a
// setting the pool cannot keep in its own clones without losing the
// connect-phase proof or the per-endpoint verification. The returned error
// wraps it and names the setting.
var ErrUnsupportedBase = errors.New("endpoint pool: unsupported base transport")

// errOwnedTransport is wrapped by assertHTTP1 when an owned clone could
// negotiate a protocol other than HTTP/1.1 or does not dial through the pool.
var errOwnedTransport = errors.New("endpoint pool: owned transport fails its construction checks")

// h2Proto is the ALPN name of HTTP/2.
const h2Proto = "h2"

// checkBase refuses a base transport whose settings the pool's clones cannot
// honor.
//
//   - A Proxy, because a failure inside a proxy tunnel cannot be attributed to
//     the connect phase of the endpoint.
//   - A DialTLSContext, because the pool replaces it with its own tagging
//     dialer and would silently drop the caller's.
//   - A MaxConnsPerHost limit, because a request that waits for a connection
//     slot starts no dial of its own, so the send-by bound on its dial could
//     not stop it from being handed a connection after its deadline.
//   - A ResponseHeaderTimeout, because each endpoint gets one clone with the
//     pool's header deadline and one without any, and a base value would be
//     silently replaced in both.
//   - A TLS VerifyPeerCertificate or VerifyConnection, because cloning it would
//     copy one host's verifier into every endpoint's clone, and a discovered
//     endpoint's clone replaces it with the pinned callback.
//   - A TLS ServerName, because each clone verifies the name of the endpoint it
//     dials, and one fixed name cannot be right for every endpoint.
//
// The SDK sets none of these, so the checks are guards, not restrictions any
// deployment meets today.
func checkBase(base *http.Transport) error {
	if base == nil {
		return fmt.Errorf("%w: the base transport is nil", ErrUnsupportedBase)
	}
	if base.Proxy != nil {
		return fmt.Errorf("%w: it sets a Proxy, and a failure inside a proxy tunnel cannot be attributed "+
			"to the connect phase", ErrUnsupportedBase)
	}
	if base.DialTLSContext != nil {
		return fmt.Errorf("%w: it sets its own DialTLSContext, which the pool's connect-phase dialer "+
			"would replace", ErrUnsupportedBase)
	}
	if base.MaxConnsPerHost > 0 {
		return fmt.Errorf("%w: it sets MaxConnsPerHost to %d, and a request waiting for a connection slot "+
			"could not be held to its send-by deadline", ErrUnsupportedBase, base.MaxConnsPerHost)
	}
	if base.ResponseHeaderTimeout > 0 {
		return fmt.Errorf("%w: it sets a ResponseHeaderTimeout of %s, and the pool chooses the header "+
			"deadline per request", ErrUnsupportedBase, base.ResponseHeaderTimeout)
	}
	if cfg := base.TLSClientConfig; cfg != nil {
		if cfg.VerifyPeerCertificate != nil {
			return fmt.Errorf("%w: its TLS config sets VerifyPeerCertificate, which would be copied into "+
				"every endpoint's clone", ErrUnsupportedBase)
		}
		if cfg.VerifyConnection != nil {
			return fmt.Errorf("%w: its TLS config sets VerifyConnection, which would be copied into "+
				"every endpoint's clone", ErrUnsupportedBase)
		}
		if cfg.ServerName != "" {
			return fmt.Errorf("%w: its TLS config fixes ServerName to %q, and each endpoint must verify "+
				"its own name", ErrUnsupportedBase, cfg.ServerName)
		}
	}
	return nil
}

// assertHTTP1 checks that an owned clone can speak only HTTP/1.1, where a
// request owns its connection, which is what the connect-phase proof assumes.
// The clone must keep ForceAttemptHTTP2 false, carry an empty non-nil
// TLSNextProto, leave Protocols nil so the transport's default of HTTP/1 alone
// applies, offer no h2 in its ALPN list, and dial through the pool's own
// DialContext and DialTLSContext.
func assertHTTP1(t *http.Transport) error {
	switch {
	case t == nil:
		return fmt.Errorf("%w: the transport is nil", errOwnedTransport)
	case t.ForceAttemptHTTP2:
		return fmt.Errorf("%w: ForceAttemptHTTP2 is set", errOwnedTransport)
	case t.TLSNextProto == nil:
		return fmt.Errorf("%w: TLSNextProto is nil, which lets the transport enable HTTP/2", errOwnedTransport)
	case len(t.TLSNextProto) != 0:
		return fmt.Errorf("%w: TLSNextProto has %d entries", errOwnedTransport, len(t.TLSNextProto))
	case t.Protocols != nil:
		return fmt.Errorf("%w: Protocols is set to %s", errOwnedTransport, t.Protocols)
	case t.TLSClientConfig != nil && slices.Contains(t.TLSClientConfig.NextProtos, h2Proto):
		return fmt.Errorf("%w: the TLS config offers h2", errOwnedTransport)
	case t.DialContext == nil || t.DialTLSContext == nil:
		return fmt.Errorf("%w: the transport does not dial through the pool's tagging dialer", errOwnedTransport)
	}
	return nil
}
