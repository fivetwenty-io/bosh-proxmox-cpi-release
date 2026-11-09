package endpointpool

import (
	"context"
	"strings"
	"time"
)

type sendByKey struct{}

// sendByMarker is one send-by deadline and the request it guards. Markers
// chain, so a second WithSendBy on the same context for another request does
// not hide the first.
type sendByMarker struct {
	deadline time.Time
	method   string
	path     string
	parent   *sendByMarker
}

// APIPrefix is the path the SDK puts in front of every API route. A request's
// URL path carries it, so the failover transport sees "/api2/json/pools" for
// the route the spec writes as "/pools".
const APIPrefix = "/api2/json"

// apiRelative returns path without the leading APIPrefix, so a marker written
// as "/pools" and a request path of "/api2/json/pools" compare equal. A path
// that does not start with the prefix followed by a slash comes back as is.
func apiRelative(path string) string {
	if rest, ok := strings.CutPrefix(path, APIPrefix+"/"); ok {
		return "/" + rest
	}
	return path
}

func (m *sendByMarker) matches(method, path string) bool {
	return strings.EqualFold(m.method, method) && m.path == apiRelative(path)
}

// WithSendBy returns a context that carries a send-by deadline for the request
// with the given method and URL path. The deadline bounds the moment the
// request's bytes are written. The failover transport checks it before every
// hop, uses it to bound connection setup, and refuses to send with
// ErrSendByPassed once it has passed.
//
// The marker names the one request it guards, so a read on the same context,
// such as a readback after the guarded call, ignores it. The path is the
// API-relative route without the query string, such as "/pools". A leading
// APIPrefix on the marker's path or on the request's path is ignored, so the
// marker matches the "/api2/json/pools" path that a transport sees. A marker with an empty method or path never
// matches anything. When the context already carries markers they stay, and a
// request that matches several is held to the earliest deadline.
func WithSendBy(ctx context.Context, deadline time.Time, method, path string) context.Context {
	if method == "" || path == "" {
		return ctx
	}
	parent, _ := ctx.Value(sendByKey{}).(*sendByMarker)
	return context.WithValue(ctx, sendByKey{}, &sendByMarker{
		deadline: deadline,
		method:   strings.ToUpper(method),
		path:     apiRelative(path),
		parent:   parent,
	})
}

// SendBy returns the send-by deadline that applies to the request with the
// given method and path, which may carry the APIPrefix. It reports false when the context carries no
// marker that matches the request. The method comparison ignores case.
func SendBy(ctx context.Context, method, path string) (time.Time, bool) {
	var (
		found    bool
		earliest time.Time
	)
	m, _ := ctx.Value(sendByKey{}).(*sendByMarker)
	for ; m != nil; m = m.parent {
		if !m.matches(method, path) {
			continue
		}
		if !found || m.deadline.Before(earliest) {
			earliest = m.deadline
			found = true
		}
	}
	return earliest, found
}

// CheckSendBy returns ErrSendByPassed when the context carries a send-by
// deadline that matches the request and now is at or after it. A deadline that
// has been reached counts as passed, so the check fails closed. It
// returns nil for a request no marker guards.
func CheckSendBy(ctx context.Context, now time.Time, method, path string) error {
	deadline, ok := SendBy(ctx, method, path)
	if !ok {
		return nil
	}
	if !now.Before(deadline) {
		return ErrSendByPassed
	}
	return nil
}
